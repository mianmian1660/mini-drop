# ============================================================
# flamegraph.py — CPU 火焰图生成器 小白版注释
# ============================================================
# 这个模块封装了 "perf.data → SVG 火焰图" 的完整流水线：
#   perf script → stackcollapse-perf.pl → flamegraph.pl → SVG
#
# 依赖：
#   - perf (Linux 系统工具)
#   - perl (执行 .pl 脚本)
#   - stackcollapse-perf.pl (Brendan Gregg 的折叠栈脚本)
#   - flamegraph.pl (Brendan Gregg 的火焰图脚本)
#
# Python 语法小课堂：
#   subprocess.run()  = 执行外部命令，等待完成
#   subprocess.PIPE   = 捕获命令的输出
#   check=True        = 命令失败时抛异常
# ============================================================

import os
import subprocess
import sys
import tempfile
from typing import Optional


# ----------------------------------------------------------
# 脚本路径（相对于本文件所在目录）
# ----------------------------------------------------------
_SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
STACKCOLLAPSE_SCRIPT = os.path.join(_SCRIPT_DIR, "stackcollapse-perf.pl")
FLAMEGRAPH_SCRIPT = os.path.join(_SCRIPT_DIR, "flamegraph.pl")


def _check_dependencies():
    """
    检查依赖工具是否可用，不可用则打印警告
    """
    # 检查 perf
    if not _which("perf"):
        print("[flamegraph] 警告: perf 未安装或不在 PATH 中", file=sys.stderr)

    # 检查 perl
    if not _which("perl"):
        print("[flamegraph] 警告: perl 未安装或不在 PATH 中", file=sys.stderr)

    # 检查脚本
    for script, name in [(STACKCOLLAPSE_SCRIPT, "stackcollapse-perf.pl"),
                          (FLAMEGRAPH_SCRIPT, "flamegraph.pl")]:
        if not os.path.exists(script):
            print(f"[flamegraph] 警告: {name} 不存在 ({script})", file=sys.stderr)
        elif not os.access(script, os.X_OK):
            print(f"[flamegraph] 警告: {name} 没有执行权限，尝试 chmod +x", file=sys.stderr)
            os.chmod(script, 0o755)


def _which(cmd: str) -> bool:
    """检查命令是否在 PATH 中"""
    return subprocess.call(["which", cmd],
                           stdout=subprocess.DEVNULL,
                           stderr=subprocess.DEVNULL) == 0


# ----------------------------------------------------------
# run_perf_script — 把 perf.data 转成可读文本
# ----------------------------------------------------------
def run_perf_script(perf_data_path: str) -> str:
    """
    执行: perf script -i <perf_data_path>

    参数:
        perf_data_path: perf.data 文件路径

    返回:
        perf script 的标准输出（调用栈文本）

    异常:
        subprocess.CalledProcessError: perf 命令失败
        FileNotFoundError: perf 未安装
    """
    print(f"[flamegraph] 执行 perf script -i {perf_data_path} ...", file=sys.stderr)
    result = subprocess.run(
        ["perf", "script", "-i", perf_data_path],
        capture_output=True, text=True,
        timeout=120  # 大文件可能较慢
    )
    if result.returncode != 0:
        error_msg = result.stderr.strip() or "perf script 返回非零退出码"
        raise subprocess.CalledProcessError(
            result.returncode, ["perf", "script", "-i", perf_data_path],
            output=result.stdout, stderr=result.stderr
        )

    lines = result.stdout.strip()
    if not lines:
        print("[flamegraph] 警告: perf script 输出为空（perf.data 可能不含有效调用栈）",
              file=sys.stderr)

    print(f"[flamegraph] perf script 输出 {len(lines)} 字符", file=sys.stderr)
    return lines


# ----------------------------------------------------------
# run_stackcollapse — 折叠调用栈
# ----------------------------------------------------------
def run_stackcollapse(perf_script_output: str) -> str:
    """
    执行: stackcollapse-perf.pl
    把 perf script 的多行调用栈折叠成一行一栈的格式

    输入格式:
        python3 12345 ...:
            func1 (lib)
            func2 (lib)

    输出格式:
        func1;func2 count

    参数:
        perf_script_output: perf script 的输出文本

    返回:
        折叠后的栈文本
    """
    print(f"[flamegraph] 执行 stackcollapse-perf.pl ...", file=sys.stderr)
    result = subprocess.run(
        ["perl", STACKCOLLAPSE_SCRIPT],
        input=perf_script_output,
        capture_output=True, text=True,
        timeout=60
    )
    if result.returncode != 0:
        error_msg = result.stderr.strip() or "stackcollapse 返回非零退出码"
        raise subprocess.CalledProcessError(
            result.returncode, ["perl", STACKCOLLAPSE_SCRIPT],
            output=result.stdout, stderr=result.stderr
        )

    lines = result.stdout.strip()
    line_count = len(lines.split("\n")) if lines else 0
    print(f"[flamegraph] 折叠栈输出 {line_count} 行", file=sys.stderr)
    return lines


# ----------------------------------------------------------
# run_flamegraph — 生成 SVG 火焰图
# ----------------------------------------------------------
def run_flamegraph(folded_stacks: str, title: str = "Flame Graph",
                   width: int = 1200, colors: str = "hot") -> str:
    """
    执行: flamegraph.pl --title "xxx" --width 1200 --colors hot
    把折叠栈文本渲染成交互式 SVG 火焰图

    参数:
        folded_stacks: 折叠后的栈文本
        title:         火焰图标题
        width:         SVG 宽度（像素）
        colors:        配色方案: hot / java / js / green / blue / aqua / wakeup / io / perl / mem

    返回:
        SVG 字符串
    """
    print(f"[flamegraph] 生成火焰图 SVG (title='{title}', width={width}) ...", file=sys.stderr)
    result = subprocess.run(
        ["perl", FLAMEGRAPH_SCRIPT,
         "--title", title,
         "--width", str(width),
         "--colors", colors],
        input=folded_stacks,
        capture_output=True, text=True,
        timeout=60
    )
    if result.returncode != 0:
        error_msg = result.stderr.strip() or "flamegraph 返回非零退出码"
        raise subprocess.CalledProcessError(
            result.returncode,
            ["perl", FLAMEGRAPH_SCRIPT, "--title", title],
            output=result.stdout, stderr=result.stderr
        )

    svg = result.stdout
    svg_size = len(svg)
    print(f"[flamegraph] SVG 生成完成 ({svg_size} bytes)", file=sys.stderr)
    return svg


# ----------------------------------------------------------
# generate_flamegraph — 一键生成火焰图（完整流水线）
# ----------------------------------------------------------
def generate_flamegraph(perf_data_path: str,
                        title: str = "CPU Flame Graph",
                        width: int = 1200,
                        colors: str = "hot") -> str:
    """
    一键生成火焰图 SVG：perf.data → SVG
    支持两种输入：
      1. 标准 perf.data（通过 perf script 解析）
      2. 已折叠的栈数据（以分号分隔，直接跳过 perf script）

    用法:
        svg = generate_flamegraph("/path/to/perf.data", title="my app")
        with open("flamegraph.svg", "w") as f:
            f.write(svg)

    参数:
        perf_data_path: perf.data 文件路径（或已折叠栈文件）
        title:          火焰图标题
        width:          SVG 宽度
        colors:         配色方案

    返回:
        SVG 字符串

    异常:
        FileNotFoundError: 依赖工具未安装
        subprocess.CalledProcessError: 流水线中某个步骤失败
    """
    # 检查依赖（首次调用时）
    _check_dependencies()

    print(f"[flamegraph] ===== 开始生成火焰图 =====", file=sys.stderr)
    print(f"[flamegraph] 输入: {perf_data_path}", file=sys.stderr)

    # 智能检测：如果文件内容已经是折叠栈格式（含分号分隔），跳过 perf script
    folded = _detect_and_fold(perf_data_path)

    # 步骤 3: flamegraph
    svg = run_flamegraph(folded, title=title, width=width, colors=colors)

    print(f"[flamegraph] ===== 火焰图生成完毕 =====", file=sys.stderr)
    return svg


def _detect_and_fold(perf_data_path: str) -> str:
    """智能折叠：检测输入格式，自动选择处理方式"""
    try:
        with open(perf_data_path, 'r', errors='replace') as f:
            first_line = f.readline().strip()
            # 如果第一行包含分号，说明已经是 collapsed stacks 格式
            if ';' in first_line and ' ' in first_line:
                f.seek(0)
                folded = f.read()
                print(f"[flamegraph] 检测到已折叠栈格式（{len(folded)} 字节），跳过 perf script",
                      file=sys.stderr)
                return folded
    except Exception:
        pass

    # 标准 perf.data 流程
    script_output = run_perf_script(perf_data_path)
    return run_stackcollapse(script_output)


# ----------------------------------------------------------
# get_folded_stacks — 只执行前两步，返回折叠栈（不给 flamegraph）
# ----------------------------------------------------------
def get_folded_stacks(perf_data_path: str) -> str:
    """
    只执行 perf script → stackcollapse，返回折叠栈文本
    用于后续的热点分析（TopN 计算等）

    参数:
        perf_data_path: perf.data 文件路径

    返回:
        折叠后的栈文本
    """
    _check_dependencies()
    return _detect_and_fold(perf_data_path)
