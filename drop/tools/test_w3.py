#!/usr/bin/env python3
"""
W3 全链路测试脚本
模拟 grpcurl 调用 ControlService.CreateTask
验证：CreateTask → server队列 → agent心跳拉取 → perf采集 → NotifyResult
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


def test_full_chain():
    """测试完整 W3 链路"""
    DROP_DIR = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "build")
    
    print("=" * 60)
    print("W3 全链路测试")
    print("=" * 60)
    
    # 1. 启动 drop_server
    print("\n[1/5] 启动 drop_server...")
    server_log = open("/tmp/server_w3.log", "w")
    server_proc = subprocess.Popen(
        [os.path.join(DROP_DIR, "drop_server")],
        stdout=server_log,
        stderr=subprocess.STDOUT,
    )
    time.sleep(2)
    
    if server_proc.poll() is not None:
        print("❌ drop_server 启动失败!")
        return False
    print("✅ drop_server 启动成功 (PID=%d)" % server_proc.pid)
    
    # 2. 启动 drop_agent
    print("\n[2/5] 启动 drop_agent...")
    agent_log = open("/tmp/agent_w3.log", "w")
    agent_proc = subprocess.Popen(
        [os.path.join(DROP_DIR, "drop_agent"), "localhost:50051"],
        stdout=agent_log,
        stderr=subprocess.STDOUT,
    )
    time.sleep(3)
    
    if agent_proc.poll() is not None:
        print("❌ drop_agent 启动失败!")
        server_proc.terminate()
        return False
    print("✅ drop_agent 启动成功 (PID=%d)" % agent_proc.pid)
    
    # 3. 通过 gRPC 调用 CreateTask
    print("\n[3/5] 通过 gRPC 调用 ControlService.CreateTask...")
    
    channel = grpc.insecure_channel('localhost:50051')
    stub = control_pb2_grpc.ControlStub(channel)
    
    task_desc = hotmethod_pb2.TaskDesc()
    task_desc.taskType = 0
    task_desc.profilerType = 0
    task_desc.timeoutSec = 30
    
    argv = task_desc.sampleArgv
    argv.hz = 99
    argv.duration = 10
    argv.pid = -1
    argv.callgraph = "fp"
    argv.event = "cpu-cycles"
    
    request = control_pb2.CreateTaskRequest()
    request.targetIP = "127.0.0.1"
    request.service = "hotmethod"
    request.taskDesc.CopyFrom(task_desc)
    
    try:
        response = stub.CreateTask(request, timeout=10)
        task_id = response.taskID
        print(f"✅ CreateTask 成功!")
        print(f"   taskID: {task_id}")
        print(f"   code: {response.code}")
        print(f"   msg: {response.msg}")
    except grpc.RpcError as e:
        print(f"❌ CreateTask 失败: {e.code()} - {e.details()}")
        server_proc.terminate()
        agent_proc.terminate()
        return False
    
    # 4. 等待 agent 通过心跳拿到任务并执行 perf
    print(f"\n[4/5] 等待 agent 执行采集（约 15 秒）...")
    time.sleep(15)
    
    # 5. 检查日志验证全链路
    print("\n[5/5] 检查日志验证全链路...")
    time.sleep(2)
    
    # 终止进程
    server_proc.terminate()
    agent_proc.terminate()
    time.sleep(1)
    try:
        server_proc.wait(timeout=5)
        agent_proc.wait(timeout=5)
    except subprocess.TimeoutExpired:
        server_proc.kill()
        agent_proc.kill()
    
    server_log.close()
    agent_log.close()
    
    # 读取日志
    with open("/tmp/server_w3.log", "r") as f:
        server_output = f.read()
    with open("/tmp/agent_w3.log", "r") as f:
        agent_output = f.read()
    
    print("\n--- Server 日志 ---")
    print(server_output[-2000:] if len(server_output) > 2000 else server_output)
    
    print("\n--- Agent 日志 ---")
    print(agent_output[-2000:] if len(agent_output) > 2000 else agent_output)
    
    # 验证关键步骤
    checks = {
        "CreateTask接收": "[server] CreateTask:" in server_output,
        "任务入队": "任务入队:" in server_output,
        "心跳带任务": "pending=1" in agent_output,
        "收到任务": "收到任务!" in agent_output,
        "perf执行退出": "perf 子进程退出" in agent_output,
        "采集成功": "采集成功" in agent_output,
        "NotifyResult上报": "NotifyResult 上报" in agent_output,
        "Server收到NotfiyResult": "收到结果:" in server_output,
    }
    
    print("\n" + "=" * 60)
    print("检查结果:")
    all_passed = True
    for check_name, result in checks.items():
        status = "✅" if result else "❌"
        if not result:
            all_passed = False
        print(f"  {status} {check_name}")
    
    if all_passed:
        print("\n🎉 W3 全链路测试通过!")
    else:
        print("\n⚠️  W3 全链路测试部分失败，需要修复")
    
    # 检查 perf.data 是否生成
    perf_files = []
    for f in os.listdir("/tmp/"):
        if f.startswith("perf_task-") or f.startswith("perf_w2-"):
            perf_files.append(os.path.join("/tmp", f))
    if perf_files:
        print(f"\n📁 生成的 perf.data 文件:")
        for f in perf_files:
            size = os.path.getsize(f)
            print(f"   {f} ({size} bytes)")
    else:
        print("\n⚠️  未找到 perf.data 输出文件")
    
    print("=" * 60)
    return all_passed


if __name__ == "__main__":
    success = test_full_chain()
    sys.exit(0 if success else 1)
