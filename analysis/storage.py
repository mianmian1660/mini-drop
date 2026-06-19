# ============================================================
# storage.py — 对象存储抽象层 小白版注释
# ============================================================
# 这个文件定义了统一的存储接口，方便切换 MinIO / COS / S3
# 当前 MVP 阶段：仅实现 MinIO
#
# Python 语法小课堂：
#   class XXX:            = 定义一个类
#   def xxx(self, ...):   = 定义类的方法（self = 对象自身）
#   @abc.abstractmethod   = 抽象方法，子类必须实现
#   try: ... except: ...  = 异常处理
#   with xxx as f:        = 上下文管理器，自动关闭资源
# ============================================================

import abc
import io
import os
import sys
import time
from typing import Optional, List

# MinIO Python SDK
from minio import Minio
from minio.error import S3Error


# ----------------------------------------------------------
# FileInfo — 文件基本信息
# ----------------------------------------------------------
class FileInfo:
    def __init__(self, name: str, size: int = 0,
                 last_modified: Optional[float] = None,
                 content_type: str = "application/octet-stream"):
        self.name = name
        self.size = size
        self.last_modified = last_modified or time.time()
        self.content_type = content_type

    def __repr__(self):
        return f"FileInfo(name={self.name}, size={self.size})"


# ----------------------------------------------------------
# Storage — 对象存储抽象基类
# ----------------------------------------------------------
class Storage(abc.ABC):
    """对象存储接口，所有后端（MinIO/COS/S3）必须实现此接口"""

    @abc.abstractmethod
    def ensure_bucket(self, bucket: str) -> bool:
        """确保存储桶存在，不存在则创建"""
        ...

    @abc.abstractmethod
    def put_object(self, bucket: str, key: str,
                   data: bytes, content_type: str = "application/octet-stream") -> bool:
        """上传文件到指定路径"""
        ...

    @abc.abstractmethod
    def get_object(self, bucket: str, key: str) -> Optional[bytes]:
        """下载文件，返回文件内容"""
        ...

    @abc.abstractmethod
    def get_object_stream(self, bucket: str, key: str):
        """下载文件，返回流对象（适合大文件）"""
        ...

    @abc.abstractmethod
    def presigned_get_url(self, bucket: str, key: str,
                          expires_sec: int = 900) -> Optional[str]:
        """生成预签名下载 URL（有效期秒数，默认 15 分钟）"""
        ...

    @abc.abstractmethod
    def list_objects(self, bucket: str, prefix: str = "") -> List[FileInfo]:
        """列出指定前缀下的所有文件"""
        ...

    @abc.abstractmethod
    def object_exists(self, bucket: str, key: str) -> bool:
        """检查文件是否存在"""
        ...

    @abc.abstractmethod
    def delete_object(self, bucket: str, key: str) -> bool:
        """删除文件"""
        ...


# ----------------------------------------------------------
# MinIOStorage — MinIO 对象存储实现
# ----------------------------------------------------------
class MinIOStorage(Storage):
    """
    MinIO 存储客户端

    使用方式：
        storage = MinIOStorage("localhost:9000", "drop", "dropdrop", use_ssl=False)
        storage.ensure_bucket("drop-data")
        storage.put_object("drop-data", "test.txt", b"hello")
        data = storage.get_object("drop-data", "test.txt")
    """

    def __init__(self, endpoint: str, access_key: str,
                 secret_key: str, use_ssl: bool = False):
        """
        创建 MinIO 客户端

        参数：
            endpoint:   MinIO 地址（如 localhost:9000）
            access_key: 访问密钥
            secret_key: 秘密密钥
            use_ssl:    是否使用 HTTPS
        """
        self.endpoint = endpoint
        self.client = Minio(
            endpoint,
            access_key=access_key,
            secret_key=secret_key,
            secure=use_ssl
        )
        print(f"[storage] MinIO 客户端已创建: {endpoint}", file=sys.stderr)

    # ---------- ensure_bucket ----------
    def ensure_bucket(self, bucket: str) -> bool:
        """确保存储桶存在"""
        try:
            if not self.client.bucket_exists(bucket):
                self.client.make_bucket(bucket)
                print(f"[storage] 创建 Bucket: {bucket}", file=sys.stderr)
            else:
                print(f"[storage] Bucket 已存在: {bucket}", file=sys.stderr)
            return True
        except S3Error as e:
            print(f"[storage] Bucket 操作失败: {e}", file=sys.stderr)
            return False

    # ---------- put_object ----------
    def put_object(self, bucket: str, key: str,
                   data: bytes, content_type: str = "application/octet-stream") -> bool:
        """上传文件"""
        try:
            self.client.put_object(
                bucket, key,
                io.BytesIO(data), len(data),
                content_type=content_type
            )
            print(f"[storage] 上传成功: {bucket}/{key} ({len(data)} bytes)", file=sys.stderr)
            return True
        except S3Error as e:
            print(f"[storage] 上传失败: {bucket}/{key} - {e}", file=sys.stderr)
            return False

    # ---------- get_object ----------
    def get_object(self, bucket: str, key: str) -> Optional[bytes]:
        """下载文件，返回完整内容"""
        try:
            response = self.client.get_object(bucket, key)
            data = response.read()
            response.close()
            response.release_conn()
            print(f"[storage] 下载成功: {bucket}/{key} ({len(data)} bytes)", file=sys.stderr)
            return data
        except S3Error as e:
            print(f"[storage] 下载失败: {bucket}/{key} - {e}", file=sys.stderr)
            return None

    # ---------- get_object_stream ----------
    def get_object_stream(self, bucket: str, key: str):
        """下载文件，返回流对象（适合大文件，不用全部加载到内存）"""
        try:
            response = self.client.get_object(bucket, key)
            print(f"[storage] 打开流: {bucket}/{key}", file=sys.stderr)
            return response  # 调用者用完要 close + release_conn
        except S3Error as e:
            print(f"[storage] 打开流失败: {bucket}/{key} - {e}", file=sys.stderr)
            return None

    # ---------- presigned_get_url ----------
    def presigned_get_url(self, bucket: str, key: str,
                          expires_sec: int = 900) -> Optional[str]:
        """生成预签名下载 URL"""
        try:
            from datetime import timedelta
            url = self.client.presigned_get_object(
                bucket, key,
                expires=timedelta(seconds=expires_sec)
            )
            return url
        except S3Error as e:
            print(f"[storage] 生成签名 URL 失败: {bucket}/{key} - {e}", file=sys.stderr)
            return None

    # ---------- list_objects ----------
    def list_objects(self, bucket: str, prefix: str = "") -> List[FileInfo]:
        """列出指定前缀下的所有文件"""
        files = []
        try:
            objects = self.client.list_objects(bucket, prefix=prefix, recursive=True)
            for obj in objects:
                files.append(FileInfo(
                    name=obj.object_name,
                    size=obj.size,
                    last_modified=obj.last_modified.timestamp() if obj.last_modified else None,
                    content_type=obj.content_type or "application/octet-stream"
                ))
            print(f"[storage] 列出 {len(files)} 个文件: {bucket}/{prefix}", file=sys.stderr)
        except S3Error as e:
            print(f"[storage] 列文件失败: {bucket}/{prefix} - {e}", file=sys.stderr)
        return files

    # ---------- object_exists ----------
    def object_exists(self, bucket: str, key: str) -> bool:
        """检查文件是否存在"""
        try:
            self.client.stat_object(bucket, key)
            return True
        except S3Error:
            return False

    # ---------- delete_object ----------
    def delete_object(self, bucket: str, key: str) -> bool:
        """删除文件"""
        try:
            self.client.remove_object(bucket, key)
            print(f"[storage] 删除成功: {bucket}/{key}", file=sys.stderr)
            return True
        except S3Error as e:
            print(f"[storage] 删除失败: {bucket}/{key} - {e}", file=sys.stderr)
            return False


# ----------------------------------------------------------
# 工厂函数：根据配置创建存储客户端
# ----------------------------------------------------------
def create_storage(config: dict) -> Storage:
    """
    根据配置字典创建存储客户端

    配置格式：
        {
            "endpoint": "localhost:9000",
            "access_key": "drop",
            "secret_key": "dropdrop",
            "use_ssl": false,
            "bucket": "drop-data"
        }
    """
    endpoint = config.get("endpoint", "localhost:9000")
    access_key = config.get("access_key", "drop")
    secret_key = config.get("secret_key", "dropdrop")
    use_ssl = config.get("use_ssl", False)

    # 支持环境变量覆盖
    endpoint = os.environ.get("S3_ENDPOINT", endpoint)
    access_key = os.environ.get("S3_ACCESS_KEY", access_key)
    secret_key = os.environ.get("S3_SECRET_KEY", secret_key)

    return MinIOStorage(endpoint, access_key, secret_key, use_ssl)
