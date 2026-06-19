# ============================================================
# error.py — 错误码定义 小白版注释
# ============================================================
# 这个文件定义了分析引擎的所有错误码和 ErrorInfo 结构体
# apiserver 通过 stderr 读取 ErrorInfo JSON 来判断分析结果
#
# 退出码约定（与 apiserver 的契约）：
#   0   = 成功
#   非0 = 失败（同时 stderr 里输出 ErrorInfo JSON）
# ============================================================

import json
import sys


# ----------------------------------------------------------
# 错误码常量
# ----------------------------------------------------------
class ErrorCode:
    """错误码枚举"""
    OK = 0                      # 成功
    ERR_DB_CONNECT = 1001       # 数据库连接失败
    ERR_DB_QUERY = 1002         # 数据库查询失败
    ERR_DB_UPDATE = 1003        # 数据库更新失败
    ERR_STORAGE_CONNECT = 2001  # 对象存储连接失败
    ERR_DOWNLOAD_FAILED = 2002  # 文件下载失败
    ERR_UPLOAD_FAILED = 2003    # 文件上传失败
    ERR_FILE_NOT_FOUND = 2004   # 文件不存在
    ERR_TASK_NOT_FOUND = 3001   # 任务不存在
    ERR_TASK_STATUS = 3002      # 任务状态异常
    ERR_ANALYSIS_FAILED = 4001  # 分析执行失败
    ERR_UNKNOWN_TYPE = 4002     # 不支持的任务类型
    ERR_CONFIG = 5001           # 配置错误


# ----------------------------------------------------------
# ErrorInfo — 错误信息结构体
# ----------------------------------------------------------
class ErrorInfo:
    """
    错误信息，通过 stderr 输出为 JSON，供 apiserver 读取

    使用方式：
        err = ErrorInfo(ErrorCode.ERR_DOWNLOAD_FAILED, "perf.data 下载失败")
        err.write_stderr()
        sys.exit(1)
    """

    def __init__(self, code: int, message: str, detail: str = ""):
        self.code = code
        self.message = message
        self.detail = detail

    def to_dict(self) -> dict:
        """转为字典"""
        d = {"code": self.code, "message": self.message}
        if self.detail:
            d["detail"] = self.detail
        return d

    def to_json(self) -> str:
        """转为 JSON 字符串"""
        return json.dumps(self.to_dict(), ensure_ascii=False)

    def write_stderr(self):
        """把错误信息写到 stderr"""
        print(self.to_json(), file=sys.stderr)


# ----------------------------------------------------------
# 便捷函数
# ----------------------------------------------------------
def exit_ok(result: dict = None):
    """
    成功退出
    参数 result 会被序列化为 JSON 输出到 stdout
    """
    if result is None:
        result = {"status": "success"}
    print(json.dumps(result, ensure_ascii=False))
    sys.exit(0)


def exit_error(code: int, message: str, detail: str = ""):
    """
    失败退出：往 stderr 写 ErrorInfo JSON，然后以非 0 退出码退出
    """
    ErrorInfo(code, message, detail).write_stderr()
    sys.exit(1)
