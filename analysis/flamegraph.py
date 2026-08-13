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
import re
import subprocess
import sys
import tarfile
import tempfile
from typing import Optional


# ----------------------------------------------------------
# 脚本路径（相对于本文件所在目录）
# ----------------------------------------------------------
_SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
STACKCOLLAPSE_SCRIPT = os.path.join(_SCRIPT_DIR, "stackcollapse-perf.pl")
FLAMEGRAPH_SCRIPT = os.path.join(_SCRIPT_DIR, "flamegraph.pl")
_FOLDED_STACK_LINE_RE = re.compile(r"^.+\s+\d+(?:\.\d+)?$")
PERF_SCRIPT_FIELDS = "comm,pid,tid,time,event,ip,sym,dso"


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
def run_perf_script(perf_data_path: str, kallsyms_path: str = None) -> str:
    """
    执行: perf script -i <perf_data_path> [--kallsyms=<kallsyms_path>]

    参数:
        perf_data_path: perf.data 文件路径
        kallsyms_path:  Agent 采集时快照的 /proc/kallsyms。

            为什么必须由 Agent 传过来：读取 /proc/kallsyms 里的真实内核地址需要
            CAP_SYSLOG，仅 kptr_restrict=0 是不够的。analysis 容器不是特权容器，
            它本地的 /proc/kallsyms 地址全是 0，perf 认不出来，内核帧只会显示
            [unknown]。Agent 是特权容器，读得到真实地址，所以由它快照上传。

            传 None 时退回旧行为（perf 自行寻找 kallsyms），内核符号大概率解析不出来。

    返回:
        perf script 的标准输出（调用栈文本）

    异常:
        subprocess.CalledProcessError: perf 命令失败
        FileNotFoundError: perf 未安装
    """
    cmd = ["perf", "script", "-F", PERF_SCRIPT_FIELDS, "-i", perf_data_path]
    if kallsyms_path and os.path.exists(kallsyms_path):
        # perf 会主动校验该路径，文件不存在会直接报 "Invalid file"，
        # 所以这里先确认存在再拼，避免把一次可降级的缺失变成硬失败。
        cmd += [f"--kallsyms={kallsyms_path}"]
    else:
        print("[flamegraph] 警告: 未提供 kallsyms 快照，内核符号可能无法解析",
              file=sys.stderr)

    print(f"[flamegraph] 执行 {' '.join(cmd)} ...", file=sys.stderr)
    result = subprocess.run(
        cmd,
        capture_output=True, text=True,
        timeout=120  # 大文件可能较慢
    )
    if result.returncode != 0:
        error_msg = result.stderr.strip() or "perf script 返回非零退出码"
        raise subprocess.CalledProcessError(
            result.returncode, cmd,
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
                        colors: str = "hot",
                        kallsyms_path: str = None,
                        symbol_archive: str = None) -> str:
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
        kallsyms_path:  Agent 快照的 /proc/kallsyms，解析内核符号用
        symbol_archive: Agent 打包的 build-id 符号缓存，解析用户态符号用

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

    # 必须在 perf script 之前解包：perf 是在解析时才去 build-id 缓存里找符号的
    _install_symbol_archive(symbol_archive)

    # 智能检测：如果文件内容已经是折叠栈格式（含分号分隔），跳过 perf script
    folded = _detect_and_fold(perf_data_path, kallsyms_path)

    # 步骤 3: flamegraph
    svg = run_flamegraph(folded, title=title, width=width, colors=colors)

    print(f"[flamegraph] ===== 火焰图生成完毕 =====", file=sys.stderr)
    return svg


# 已解开过的符号包路径。解包是往 ~/.debug 写文件的进程级副作用，
# 同一次分析里 SVG 和 TopN 两条路径都会调用，这里去重避免重复解压。
_INSTALLED_ARCHIVES = set()


def _install_symbol_archive(archive_path: str) -> bool:
    """
    把 Agent 传来的 build-id 符号缓存解开到本地，供 perf script 查符号

    perf 解析用户态地址时会去 $HOME/.debug/.build-id 下按 build-id 找对应
    二进制。analysis 容器里没有被采集进程的二进制，所以要靠 Agent 打包传来。

    实测要点（2026-08-13 容器内验证）：
      - `perf archive` 子命令在镜像里不存在，Agent 侧改用 tar 打包，这里对应解包
      - perf record 会自动填充该缓存，无需额外 buildid-cache 操作
      - strip 过的二进制解包后仍解析不出符号，属已知局限

    解包失败只降级不抛异常——用户态符号显示为 [模块名]，但内核符号和
    火焰图本身仍然可用。

    返回: True=已解开, False=跳过或失败
    """
    if not archive_path or not os.path.exists(archive_path):
        print("[flamegraph] 警告: 未提供符号包，用户态符号可能无法解析",
              file=sys.stderr)
        return False

    # SVG 和 TopN 两条路径各调一次，同一个包重复解开纯属浪费
    if archive_path in _INSTALLED_ARCHIVES:
        return True

    buildid_dir = os.path.join(os.path.expanduser("~"), ".debug")
    try:
        os.makedirs(buildid_dir, exist_ok=True)
        with tarfile.open(archive_path, "r:gz") as tar:
            # 逐个校验成员路径，拒绝 ../ 或绝对路径逃逸出 buildid_dir。
            # 符号包来自 Agent，但仍按不可信输入处理。
            safe = []
            for member in tar.getmembers():
                target = os.path.realpath(os.path.join(buildid_dir, member.name))
                if target == buildid_dir or target.startswith(buildid_dir + os.sep):
                    safe.append(member)
                else:
                    print(f"[flamegraph] 跳过越界的符号包条目: {member.name}",
                          file=sys.stderr)
            tar.extractall(buildid_dir, members=safe)
        print(f"[flamegraph] 符号包已解开到 {buildid_dir}（{len(safe)} 个条目）",
              file=sys.stderr)
        _INSTALLED_ARCHIVES.add(archive_path)
        return True
    except Exception as e:
        print(f"[flamegraph] 解开符号包失败（降级继续）: {e}", file=sys.stderr)
        return False


def _detect_and_fold(perf_data_path: str, kallsyms_path: str = None) -> str:
    """智能折叠：检测输入格式，自动选择处理方式"""
    try:
        with open(perf_data_path, 'r', errors='replace') as f:
            sample = []
            for _ in range(30):
                line = f.readline()
                if not line:
                    break
                line = line.strip()
                if line:
                    sample.append(line)
            # folded stacks 允许单帧行（例如 "0xabc 1"），不能只靠分号判断。
            if _looks_like_folded_stacks(sample):
                f.seek(0)
                folded = f.read()
                print(f"[flamegraph] 检测到已折叠栈格式（{len(folded)} 字节），跳过 perf script",
                      file=sys.stderr)
                return folded
    except Exception:
        pass

    # 标准 perf.data 流程
    script_output = run_perf_script(perf_data_path, kallsyms_path)
    return run_stackcollapse(script_output)


def _looks_like_folded_stacks(lines) -> bool:
    if not lines:
        return False
    matches = 0
    for line in lines:
        if _FOLDED_STACK_LINE_RE.match(line):
            matches += 1
    return matches >= max(1, len(lines) // 2)


# ----------------------------------------------------------
# get_folded_stacks — 只执行前两步，返回折叠栈（不给 flamegraph）
# ----------------------------------------------------------
def get_folded_stacks(perf_data_path: str, kallsyms_path: str = None,
                      symbol_archive: str = None) -> str:
    """
    只执行 perf script → stackcollapse，返回折叠栈文本
    用于后续的热点分析（TopN 计算等）

    参数:
        perf_data_path: perf.data 文件路径
        kallsyms_path:  Agent 采集时快照的 /proc/kallsyms，透传给
            _detect_and_fold 用于解析内核帧。传 None 时内核符号可能
            无法解析（详见 run_perf_script 的说明）。
        symbol_archive: Agent 打包的 build-id 符号缓存，解析用户态符号用。

    符号包为什么这里也要接：TopN 和 SVG 是两条独立的消费路径，各自调一次
    perf script。之前只有 generate_flamegraph 接了 symbol_archive，本函数
    能拿到用户态符号纯属顺序凑巧——SVG 先跑并把符号解到了 ~/.debug 这个
    进程级位置。一旦调用顺序变化或 SVG 那步被跳过，TopN 就会悄悄退回
    [模块名]。显式接进来把这层隐式依赖去掉。
    （阶段一的 kallsyms 踩过同形状的坑，见 docs/symbolization-design.md §9.1。）

    返回:
        折叠后的栈文本
    """
    _check_dependencies()
    _install_symbol_archive(symbol_archive)
    return _detect_and_fold(perf_data_path, kallsyms_path)
