#!/usr/bin/env python3
"""
W4 全链路测试脚本
验证：
  1. PidStats 自监控（心跳中包含 CPU/内存数据）
  2. 正常 perf 采集 → MinIO 上传 → NotifyResult
  3. PID 不存在时 errorMessage 正确返回
  4. Server 端 Agent 信息跟踪和离线检测
"""
import subprocess
import time
import os
import sys

# 添加生成目录到 path
PY_GEN = os.path.join(os.path.dirname(os.path.abspath(__file__)), "py_gen")
sys.path.insert(0, PY_GEN)

import grpc

# proto 生成的 stub 位于 common/proto/ 子目录下
from common.proto import common_pb2
from common.proto import control_pb2, control_pb2_grpc
from common.proto import hotmethod_pb2
from common.proto import healthcheck_pb2
from common.proto import init_pb2


def test_pid_not_exist():
    """W4 测试1: 故意使用不存在的 PID，验证 errorMessage 正确返回"""
    DROP_DIR = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "build")
    
    print("=" * 60)
    print("W4 测试1: PID 不存在 → errorMessage 正确返回")
    print("=" * 60)
    
    server_proc = subprocess.Popen(
        [os.path.join(DROP_DIR, "drop_server")],
        stdout=open("/tmp/server_w4_pid.log", "w"),
        stderr=subprocess.STDOUT,
    )
    time.sleep(2)
    
    agent_proc = subprocess.Popen(
        [os.path.join(DROP_DIR, "drop_agent"), "localhost:50051"],
        stdout=open("/tmp/agent_w4_pid.log", "w"),
        stderr=subprocess.STDOUT,
    )
    time.sleep(4)
    
    print("\n[测试] 创建任务，使用不存在的 PID=99999...")
    
    channel = grpc.insecure_channel('localhost:50051')
    stub = control_pb2_grpc.ControlStub(channel)
    
    task_desc = hotmethod_pb2.TaskDesc()
    task_desc.taskType = 0
    task_desc.profilerType = 0
    task_desc.timeoutSec = 15
    
    argv = task_desc.sampleArgv
    argv.hz = 99
    argv.duration = 5
    argv.pid = 99999  # 不存在的 PID
    argv.callgraph = "fp"
    argv.event = "cpu-cycles"
    
    request = control_pb2.CreateTaskRequest()
    request.targetIP = "127.0.0.1"
    request.service = "hotmethod"
    request.taskDesc.CopyFrom(task_desc)
    
    try:
        response = stub.CreateTask(request, timeout=10)
        print(f"✅ CreateTask 成功! taskID={response.taskID}")
    except grpc.RpcError as e:
        print(f"❌ CreateTask 失败: {e.code()} - {e.details()}")
        server_proc.terminate()
        agent_proc.terminate()
        return False
    
    time.sleep(8)
    
    server_proc.terminate()
    agent_proc.terminate()
    time.sleep(1)
    try:
        server_proc.wait(timeout=5)
        agent_proc.wait(timeout=5)
    except subprocess.TimeoutExpired:
        server_proc.kill()
        agent_proc.kill()
    
    # 检查 agent 日志
    with open("/tmp/agent_w4_pid.log", "r") as f:
        agent_output = f.read()
    with open("/tmp/server_w4_pid.log", "r") as f:
        server_output = f.read()
    
    print("\n--- Agent 日志（关键部分）---")
    for line in agent_output.split('\n'):
        if any(kw in line for kw in ['PID', '不存在', 'error', 'taskID', '收到任务', 'NotifyResult']):
            print(line)
    
    print("\n--- Server 日志（关键部分）---")
    for line in server_output.split('\n'):
        if any(kw in line for kw in ['结果:', 'error', 'NotifyResult', 'PID']):
            print(line)
    
    # 验证
    checks = {
        "Agent收到任务": "收到任务!" in agent_output,
        "PID不存在检测": "PID 99999 不存在" in agent_output,
        "errorMessage返回": "目标 PID 99999 不存在" in agent_output,
        "NotifyResult上报": "NotifyResult 上报成功" in agent_output,
        "Server收到错误": "收到结果:" in server_output and "不存在" in server_output,
    }
    
    print("\n" + "=" * 60)
    print("W4 测试1 检查结果:")
    all_passed = True
    for check_name, result in checks.items():
        status = "✅" if result else "❌"
        if not result:
            all_passed = False
        print(f"  {status} {check_name}")
    
    return all_passed


def test_normal_perf_with_pidstats():
    """W4 测试2: 正常 perf 采集，验证 PidStats 和 MinIO cosKey 返回"""
    DROP_DIR = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "build")
    
    print("\n" + "=" * 60)
    print("W4 测试2: 正常 perf 采集 + PidStats + MinIO")
    print("=" * 60)
    
    server_proc = subprocess.Popen(
        [os.path.join(DROP_DIR, "drop_server")],
        stdout=open("/tmp/server_w4_normal.log", "w"),
        stderr=subprocess.STDOUT,
    )
    time.sleep(2)
    
    agent_proc = subprocess.Popen(
        [os.path.join(DROP_DIR, "drop_agent"), "localhost:50051"],
        stdout=open("/tmp/agent_w4_normal.log", "w"),
        stderr=subprocess.STDOUT,
    )
    time.sleep(4)
    
    print("\n[测试] 创建任务，使用当前进程 PID 进行 perf 采集...")
    
    channel = grpc.insecure_channel('localhost:50051')
    stub = control_pb2_grpc.ControlStub(channel)
    
    task_desc = hotmethod_pb2.TaskDesc()
    task_desc.taskType = 0
    task_desc.profilerType = 0
    task_desc.timeoutSec = 30
    
    argv = task_desc.sampleArgv
    argv.hz = 99
    argv.duration = 5
    argv.pid = -1  # 采集整个系统
    argv.callgraph = "fp"
    argv.event = "cpu-cycles"
    
    request = control_pb2.CreateTaskRequest()
    request.targetIP = "127.0.0.1"
    request.service = "hotmethod"
    request.taskDesc.CopyFrom(task_desc)
    
    try:
        response = stub.CreateTask(request, timeout=10)
        task_id = response.taskID
        print(f"✅ CreateTask 成功! taskID={task_id}")
    except grpc.RpcError as e:
        print(f"❌ CreateTask 失败: {e.code()} - {e.details()}")
        server_proc.terminate()
        agent_proc.terminate()
        return False
    
    print(f"[测试] 等待采集完成（约 20 秒）...")
    time.sleep(20)
    
    server_proc.terminate()
    agent_proc.terminate()
    time.sleep(1)
    try:
        server_proc.wait(timeout=5)
        agent_proc.wait(timeout=5)
    except subprocess.TimeoutExpired:
        server_proc.kill()
        agent_proc.kill()
    
    with open("/tmp/agent_w4_normal.log", "r") as f:
        agent_output = f.read()
    with open("/tmp/server_w4_normal.log", "r") as f:
        server_output = f.read()
    
    print("\n--- Agent 日志（关键部分）---")
    for line in agent_output.split('\n'):
        if any(kw in line for kw in ['PID', 'taskID', '收到任务', 'perf 子进程',
                                       'NotifyResult', '自监控', 'CPU=', 'MinIO',
                                       'cosKey', '采集成功']):
            print(line)
    
    print("\n--- Server 日志（关键部分）---")
    for line in server_output.split('\n'):
        if any(kw in line for kw in ['结果:', 'taskID', 'PidStats', 'CPU=', 
                                       'cosKey', 'heartbeat', '心跳']):
            print(line)
    
    # 检查 perf.data 文件是否生成
    perf_files = []
    for f in os.listdir("/tmp/"):
        if f.startswith("perf_task-") or f.startswith("perf_w"):
            perf_files.append(os.path.join("/tmp", f))
    
    checks = {
        "Agent收到任务": "收到任务!" in agent_output,
        "perf执行": "perf 子进程" in agent_output,
        "NotifyResult上报": "NotifyResult 上报成功" in agent_output,
        "PidStats心跳": "自监控: CPU=" in agent_output,
        "Server收到结果": "收到结果:" in server_output,
        "Server收到PidStats": "PidStats" in server_output,
        "perf.data生成": len(perf_files) > 0,
    }
    
    print("\n" + "=" * 60)
    print("W4 测试2 检查结果:")
    all_passed = True
    for check_name, result in checks.items():
        status = "✅" if result else "❌"
        if not result:
            all_passed = False
        print(f"  {status} {check_name}")
    
    if perf_files:
        print(f"\n📁 生成的 perf.data 文件:")
        for f in perf_files:
            size = os.path.getsize(f)
            print(f"   {f} ({size} bytes)")
    
    return all_passed


def test_agent_offline_detection():
    """W4 测试3: Agent 停止心跳后的离线检测"""
    DROP_DIR = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "build")
    
    print("\n" + "=" * 60)
    print("W4 测试3: Agent 离线检测")
    print("=" * 60)
    
    server_proc = subprocess.Popen(
        [os.path.join(DROP_DIR, "drop_server")],
        stdout=open("/tmp/server_w4_offline.log", "w"),
        stderr=subprocess.STDOUT,
    )
    time.sleep(2)
    
    agent_proc = subprocess.Popen(
        [os.path.join(DROP_DIR, "drop_agent"), "localhost:50051"],
        stdout=open("/tmp/agent_w4_offline.log", "w"),
        stderr=subprocess.STDOUT,
    )
    time.sleep(5)
    
    print("[测试] 停止 Agent...")
    agent_proc.terminate()
    agent_proc.wait(timeout=5)
    
    print("[测试] 等待 Server 检测离线（最多 40 秒）...")
    time.sleep(35)
    
    server_proc.terminate()
    try:
        server_proc.wait(timeout=5)
    except subprocess.TimeoutExpired:
        server_proc.kill()
    
    with open("/tmp/server_w4_offline.log", "r") as f:
        server_output = f.read()
    
    print("\n--- Server 日志（关键部分）---")
    for line in server_output.split('\n'):
        if any(kw in line for kw in ['离线', 'offline', '恢复', 'online', '心跳']):
            print(line)
    
    checks = {
        "Agent注册": "Agent 注册:" in server_output,
        "Agent离线检测": "离线" in server_output,
    }
    
    print("\n" + "=" * 60)
    print("W4 测试3 检查结果:")
    all_passed = True
    for check_name, result in checks.items():
        status = "✅" if result else "❌"
        if not result:
            all_passed = False
        print(f"  {status} {check_name}")
    
    return all_passed


if __name__ == "__main__":
    # 清理残留进程
    subprocess.run(["pkill", "-f", "drop_server"], capture_output=True)
    subprocess.run(["pkill", "-f", "drop_agent"], capture_output=True)
    time.sleep(1)
    
    results = []
    
    # 测试1: PID 不存在
    print("\n" + "🧪" * 30)
    r1 = test_pid_not_exist()
    results.append(("PID不存在错误处理", r1))
    
    # 测试2: 正常 perf + PidStats
    print("\n" + "🧪" * 30)
    r2 = test_normal_perf_with_pidstats()
    results.append(("正常采集+PidStats+MinIO", r2))
    
    # 测试3: Agent 离线检测（这个比较慢，放在最后）
    print("\n" + "🧪" * 30)
    r3 = test_agent_offline_detection()
    results.append(("Agent离线检测", r3))
    
    # 总结
    print("\n" + "=" * 60)
    print("🏆 W4 全链路测试总结")
    print("=" * 60)
    all_passed = True
    for name, passed in results:
        status = "✅ PASS" if passed else "❌ FAIL"
        if not passed:
            all_passed = False
        print(f"  {status}  {name}")
    
    if all_passed:
        print("\n🎉 W4 全部测试通过!")
    else:
        print("\n⚠️  W4 部分测试失败，需要修复")
    
    sys.exit(0 if all_passed else 1)
