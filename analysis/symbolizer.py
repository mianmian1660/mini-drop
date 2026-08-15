#!/usr/bin/env python3
# ============================================================
# symbolizer.py — 按 build-id 安装用户态符号（阶段三）
# ============================================================
# Agent 侧已经把符号按 build-id 去重上传到 MinIO（apiserver 的
# /api/v1/symbols/check + /api/v1/symbols/:build_id 两个接口）。
# 这里做对应的读取端：查 task_build_ids 表知道这个任务需要哪些
# build-id，逐个从 MinIO 下载装进 perf 默认会去找的
# ~/.debug/.build-id/<xx>/<rest>/elf。装完之后 perf script 不需要
# 任何额外参数就能自动找到符号（这也是阶段二 symbol_archive 参数被
# 移除的原因——安装是前置步骤，不再需要显式传路径）。
#
# 和仓库里其它 storage/DB 相关的辅助函数保持同一惯例：任何失败都
# 降级返回 False/[]，不抛异常——符号装不上，用户态帧显示为 [模块名]，
# 不应该让整条分析链路失败。
# ============================================================

import os
import sys


def get_task_build_ids(conn, tid: str) -> list:
    """
    查 task_build_ids 表，返回这个任务引用了哪些 build-id。

    返回: [{"build_id": ..., "dso_path": ...}, ...]，失败返回 []
    """
    if conn is None:
        return []
    try:
        cur = conn.cursor()
        cur.execute(
            "SELECT build_id, dso_path FROM task_build_ids WHERE tid = %s",
            (tid,)
        )
        rows = cur.fetchall()
        cur.close()
        return [{"build_id": r[0], "dso_path": r[1]} for r in rows]
    except Exception as e:
        print(f"[symbolizer] 查询 task_build_ids 失败（降级继续）: {e}", file=sys.stderr)
        return []


def install_symbol(storage, bucket: str, build_id: str) -> bool:
    """
    从 MinIO 下载 symbols/{build_id}，写入 perf 的 build-id 缓存结构
    ~/.debug/.build-id/<xx>/<rest>/elf。

    build_id 理论上已经过服务端正则校验（十六进制串），这里仍按不可信
    输入处理：长度过短或包含路径分隔符的一律拒绝，防止被拼进文件路径。

    返回: True=已安装，False=跳过或失败（调用方应降级继续）
    """
    if not build_id or len(build_id) < 8 or "/" in build_id or ".." in build_id:
        print(f"[symbolizer] build_id 格式不合法，跳过: {build_id!r}", file=sys.stderr)
        return False

    key = f"symbols/{build_id}"
    try:
        if not storage.object_exists(bucket, key):
            print(f"[symbolizer] 符号未在服务端就绪: {key}", file=sys.stderr)
            return False
        data = storage.get_object(bucket, key)
        if not data:
            return False

        buildid_dir = os.path.join(os.path.expanduser("~"), ".debug", ".build-id",
                                   build_id[:2], build_id[2:])
        os.makedirs(buildid_dir, exist_ok=True)
        elf_path = os.path.join(buildid_dir, "elf")
        with open(elf_path, "wb") as f:
            f.write(data)
        print(f"[symbolizer] 已安装符号 build_id={build_id} ({len(data)} bytes)", file=sys.stderr)
        return True
    except Exception as e:
        print(f"[symbolizer] 安装符号失败（降级继续）build_id={build_id}: {e}", file=sys.stderr)
        return False


def install_symbols_for_task(conn, storage, bucket: str, tid: str) -> int:
    """
    编排函数：查这个任务需要哪些符号，逐个下载安装。

    返回: 成功安装的数量（供调用方打日志用，0 不代表出错，可能这个
    任务本来就没有需要额外安装的用户态符号）
    """
    entries = get_task_build_ids(conn, tid)
    if not entries:
        return 0

    installed = 0
    for entry in entries:
        if install_symbol(storage, bucket, entry["build_id"]):
            installed += 1
    return installed
