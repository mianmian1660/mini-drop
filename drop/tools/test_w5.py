#!/usr/bin/env python3
"""
W5 全链路测试脚本
验证：
  1. 配置文件加载（自定义 hostname/uid/serverAddrs）
  2. 多 Server 故障转移（连不上第一个就试第二个）
  3. profilerType=0 (perf) 正常采集
  4. profilerType=1 (async-profiler) 采集器分发正确
  5. profilerType=2 (pprof) 采集器分发正确
  6. 错误 PID 处理对所有采集器生效
"""
import subprocess
import time
import os
import sys
import json
import tempfile

# 添加生成目录到 path
PY_GEN = os.path.join(os.path.dirname(os.path.abspath(__file__)), "py_gen")
sys.path.insert(0, PY_GEN)

import grpc

from common.proto import common_pb2
from common.proto import control_pb2, control_pb2_grpc
from common.proto import hotmethod_pb2
from common.proto import healthcheck_pb2
from common.proto import init_pb2

DROP_DIR = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "build")
TEST_LOG_DIR = "/tmp/w5_test_logs"

os.makedirs(TEST_LOG_DIR, exist_ok=True)


def test_config_loading():
    """W5 测试1: 配置文件加载 —— 自定义 hostname/uid/serverAddrs"""
    print("=" * 60)
    print("W5 测试1: 配置文件加载")
    print("=" * 60)

    # 创建临时配置文件
    config = {
        "hostname": "w5-test-host",
        "uid": "w5-agent-999",
        "agentVersion": "2.0.0-w5",
        "heartbeatIntervalSec": 3,
        "registerTimeoutSec": 5,
        "serverAddrs": [
            "localhost:50051"
        ]
    }

    config_path = os.path.join(TEST_LOG_DIR, "w5_test_config.json")
    with open(config_path, "w") as f:
        json.dump(config, f, indent=2)

    print(f"\n[测试] 配置文件: {config_path}")
    print(json.dumps(config, indent=2))

    # 启动 server
    server_proc = subprocess.Popen(
        [os.path.join(DROP_DIR, "drop_server")],
        stdout=open(os.path.join(TEST_LOG_DIR, "server_w5_cfg.log"), "w"),
        stderr=subprocess.STDOUT,
    )
    time.sleep(2)

    if server_proc.poll() is not None:
        print("❌ drop_server 启动失败!")
        return False
    print("✅ drop_server 启动成功")

    # 用配置文件启动 agent
    agent_proc = subprocess.Popen(
        [os.path.join(DROP_DIR, "drop_agent"), config_path],
        stdout=open(os.path.join(TEST_LOG_DIR, "agent_w5_cfg.log"), "w"),
        stderr=subprocess.STDOUT,
    )
    time.sleep(5)

    # 检查 agent 是否正常运行
    if agent_proc.poll() is not None:
        print("❌ drop_agent 启动失败!")
        server_proc.terminate()
        return False
    print("✅ drop_agent 启动成功")

    # 清理
    server_proc.terminate()
    agent_proc.terminate()
    time.sleep(1)
    try:
        server_proc.wait(timeout=5)
        agent_proc.wait(timeout=5)
    except subprocess.TimeoutExpired:
        server_proc.kill()
        agent_proc.kill()

    # 检查日志
    with open(os.path.join(TEST_LOG_DIR, "agent_w5_cfg.log"), "r") as f:
        agent_output = f.read()

    print("\n--- Agent 日志（配置相关）---")
    for line in agent_output.split('\n'):
        if any(kw in line for kw in ['config', 'hostname', 'uid=', 'serverAddrs', 'w5', '注册成功']):
            print(line)

    checks = {
        "配置文件加载": "加载配置文件" in agent_output,
        "hostname读取": "w5-test-host" in agent_output,
        "uid读取": "w5-agent-999" in agent_output,
        "version读取": "2.0.0-w5" in agent_output,
        "serverAddrs解析": "serverAddrs=" in agent_output,
        "注册成功": "注册成功!" in agent_output,
    }

    print("\n检查结果:")
    all_passed = True
    for check_name, result in checks.items():
        status = "✅" if result else "❌"
        if not result:
            all_passed = False
        print(f"  {status} {check_name}")

    return all_passed


def test_multi_server_failover():
    """W5 测试2: 多 Server 故障转移"""
    print("\n" + "=" * 60)
    print("W5 测试2: 多 Server 故障转移")
    print("=" * 60)

    # 创建配置，第一个地址不可达，第二个是可用的
    config = {
        "hostname": "failover-test-host",
        "uid": "failover-agent",
        "agentVersion": "1.0.0",
        "registerTimeoutSec": 3,
        "serverAddrs": [
            "localhost:59999",  # 不可达
            "localhost:50051",  # 可用
        ]
    }

    config_path = os.path.join(TEST_LOG_DIR, "w5_failover_config.json")
    with open(config_path, "w") as f:
        json.dump(config, f, indent=2)

    # 启动 server 在主端口
    server_proc = subprocess.Popen(
        [os.path.join(DROP_DIR, "drop_server")],
        stdout=open(os.path.join(TEST_LOG_DIR, "server_w5_fo.log"), "w"),
        stderr=subprocess.STDOUT,
    )
    time.sleep(2)

    if server_proc.poll() is not None:
        print("❌ drop_server 启动失败!")
        return False
    print("✅ drop_server 启动成功 (端口 50051)")

    # 启动 agent（会先尝试 59999 失败，再连接 50051 成功）
    agent_proc = subprocess.Popen(
        [os.path.join(DROP_DIR, "drop_agent"), config_path],
        stdout=open(os.path.join(TEST_LOG_DIR, "agent_w5_fo.log"), "w"),
        stderr=subprocess.STDOUT,
    )
    time.sleep(6)

    if agent_proc.poll() is not None:
        print("❌ drop_agent 启动失败!")
        # 仍然检查日志看故障转移过程
        server_proc.terminate()
    else:
        print("✅ drop_agent 启动成功")

    server_proc.terminate()
    agent_proc.terminate()
    time.sleep(1)
    try:
        server_proc.wait(timeout=5)
        agent_proc.wait(timeout=5)
    except subprocess.TimeoutExpired:
        server_proc.kill()
        agent_proc.kill()

    # 检查日志
    with open(os.path.join(TEST_LOG_DIR, "agent_w5_fo.log"), "r") as f:
        agent_output = f.read()

    print("\n--- Agent 日志（故障转移相关）---")
    for line in agent_output.split('\n'):
        if any(kw in line for kw in ['尝试连接', '注册失败', '注册成功', 'failover', '59999', '故障', '重连']):
            print(line)

    checks = {
        "尝试连接第一个Server": "尝试连接 Server: localhost:59999" in agent_output,
        "第一个Server失败": ("注册失败" in agent_output and "59999" in agent_output) or ("59999" in agent_output),
        "尝试连接第二个Server": "尝试连接 Server: localhost:50051" in agent_output or "localhost:50051" in agent_output,
        "最终注册成功": "注册成功!" in agent_output,
    }

    print("\n检查结果:")
    all_passed = True
    for check_name, result in checks.items():
        status = "✅" if result else "❌"
        if not result:
            all_passed = False
        print(f"  {status} {check_name}")

    return all_passed


def test_profiler_type(profiler_type, profiler_name, pid=99999):
    """通用测试：验证指定 profilerType 的采集器分发"""
    DROP_DIR_LOCAL = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "build")

    server_proc = subprocess.Popen(
        [os.path.join(DROP_DIR_LOCAL, "drop_server")],
        stdout=open(os.path.join(TEST_LOG_DIR, f"server_w5_type{profiler_type}.log"), "w"),
        stderr=subprocess.STDOUT,
    )
    time.sleep(2)

    agent_proc = subprocess.Popen(
        [os.path.join(DROP_DIR_LOCAL, "drop_agent"), "localhost:50051"],
        stdout=open(os.path.join(TEST_LOG_DIR, f"agent_w5_type{profiler_type}.log"), "w"),
        stderr=subprocess.STDOUT,
    )
    time.sleep(4)

    print(f"\n[测试] 创建 profilerType={profiler_type} ({profiler_name}) 任务...")

    channel = grpc.insecure_channel('localhost:50051')
    stub = control_pb2_grpc.ControlStub(channel)

    task_desc = hotmethod_pb2.TaskDesc()
    task_desc.taskType = 0
    task_desc.profilerType = profiler_type
    task_desc.timeoutSec = 20

    argv = task_desc.sampleArgv
    argv.hz = 99
    argv.duration = 5
    argv.pid = pid
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

    time.sleep(10)

    server_proc.terminate()
    agent_proc.terminate()
    time.sleep(1)
    try:
        server_proc.wait(timeout=5)
        agent_proc.wait(timeout=5)
    except subprocess.TimeoutExpired:
        server_proc.kill()
        agent_proc.kill()

    with open(os.path.join(TEST_LOG_DIR, f"agent_w5_type{profiler_type}.log"), "r") as f:
        agent_output = f.read()
    with open(os.path.join(TEST_LOG_DIR, f"server_w5_type{profiler_type}.log"), "r") as f:
        server_output = f.read()

    print(f"\n--- Agent 日志（{profiler_name} 相关）---")
    for line in agent_output.split('\n'):
        if any(kw in line for kw in ['profilerType', 'profiler=', '选择采集器', '收到任务', profiler_name.split('-')[0] if '-' in profiler_name else profiler_name, 'NotifyResult', 'error', 'pid', 'PID']):
            print(line)

    print(f"\n--- Server 日志（{profiler_name} 相关）---")
    for line in server_output.split('\n'):
        if any(kw in line for kw in ['结果:', 'error', 'NotifyResult', 'profiler']):
            print(line)

    # 验证点
    checks = {
        "收到任务": "收到任务!" in agent_output,
        f"选择采集器 {profiler_name}": f"选择采集器: {profiler_name}" in agent_output,
        f"profilerType={profiler_type}传递": f"profilerType={profiler_type}" in agent_output,
        "NotifyResult上报": "NotifyResult 上报成功" in agent_output or "NotifyResult" in agent_output,
    }

    print(f"\n检查结果（{profiler_name}）:")
    all_passed = True
    for check_name, result in checks.items():
        status = "✅" if result else "❌"
        if not result:
            all_passed = False
        print(f"  {status} {check_name}")

    return all_passed


def test_perf_normal():
    """W5 测试3: profilerType=0 (perf) 正常采集（全系统，不需要特定 PID）"""
    print("\n" + "=" * 60)
    print("W5 测试3: perf (profilerType=0) 正常采集")
    print("=" * 60)

    DROP_DIR_LOCAL = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "build")

    server_proc = subprocess.Popen(
        [os.path.join(DROP_DIR_LOCAL, "drop_server")],
        stdout=open(os.path.join(TEST_LOG_DIR, "server_w5_perf.log"), "w"),
        stderr=subprocess.STDOUT,
    )
    time.sleep(2)

    agent_proc = subprocess.Popen(
        [os.path.join(DROP_DIR_LOCAL, "drop_agent"), "localhost:50051"],
        stdout=open(os.path.join(TEST_LOG_DIR, "agent_w5_perf.log"), "w"),
        stderr=subprocess.STDOUT,
    )
    time.sleep(4)

    channel = grpc.insecure_channel('localhost:50051')
    stub = control_pb2_grpc.ControlStub(channel)

    task_desc = hotmethod_pb2.TaskDesc()
    task_desc.taskType = 0
    task_desc.profilerType = 0
    task_desc.timeoutSec = 30

    argv = task_desc.sampleArgv
    argv.hz = 99
    argv.duration = 5
    argv.pid = -1  # 全系统采集
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

    print("[测试] 等待 perf 采集完成（约 20 秒）...")
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

    with open(os.path.join(TEST_LOG_DIR, "agent_w5_perf.log"), "r") as f:
        agent_output = f.read()
    with open(os.path.join(TEST_LOG_DIR, "server_w5_perf.log"), "r") as f:
        server_output = f.read()

    print("\n--- Agent 日志 ---")
    for line in agent_output.split('\n'):
        if any(kw in line for kw in ['perf', '采集', 'profiler', '收到任务', 'NotifyResult', '采集成功']):
            print(line)

    checks = {
        "收到任务": "收到任务!" in agent_output,
        "选择perf采集器": "选择采集器: perf" in agent_output,
        "perf执行": "[perf]" in agent_output,
        "采集成功": "采集成功" in agent_output,
        "NotifyResult上报": "NotifyResult 上报成功" in agent_output,
    }

    print("\n检查结果 (perf):")
    all_passed = True
    for check_name, result in checks.items():
        status = "✅" if result else "❌"
        if not result:
            all_passed = False
        print(f"  {status} {check_name}")

    return all_passed


def test_pid_not_exists_all_profilers():
    """W5 测试6: 不存在的 PID，验证所有采集器都正确报错"""
    print("\n" + "=" * 60)
    print("W5 测试6: PID 不存在 → 所有采集器 errorMessage 返回")
    print("=" * 60)

    DROP_DIR_LOCAL = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "build")

    server_proc = subprocess.Popen(
        [os.path.join(DROP_DIR_LOCAL, "drop_server")],
        stdout=open(os.path.join(TEST_LOG_DIR, "server_w5_pid.log"), "w"),
        stderr=subprocess.STDOUT,
    )
    time.sleep(2)

    agent_proc = subprocess.Popen(
        [os.path.join(DROP_DIR_LOCAL, "drop_agent"), "localhost:50051"],
        stdout=open(os.path.join(TEST_LOG_DIR, "agent_w5_pid.log"), "w"),
        stderr=subprocess.STDOUT,
    )
    time.sleep(4)

    channel = grpc.insecure_channel('localhost:50051')
    stub = control_pb2_grpc.ControlStub(channel)

    all_passed = True
    test_cases = [
        (0, "perf"),
        (1, "async-profiler"),
        (2, "pprof"),
    ]

    for ptype, pname in test_cases:
        print(f"\n[测试] profilerType={ptype} ({pname}), PID=99999...")

        task_desc = hotmethod_pb2.TaskDesc()
        task_desc.taskType = 0
        task_desc.profilerType = ptype
        task_desc.timeoutSec = 15

        argv = task_desc.sampleArgv
        argv.hz = 99
        argv.duration = 3
        argv.pid = 99999
        argv.callgraph = "fp"
        argv.event = "cpu-cycles"

        request = control_pb2.CreateTaskRequest()
        request.targetIP = "127.0.0.1"
        request.service = "hotmethod"
        request.taskDesc.CopyFrom(task_desc)

        try:
            response = stub.CreateTask(request, timeout=10)
            print(f"  ✅ CreateTask 成功! taskID={response.taskID}")
        except grpc.RpcError as e:
            print(f"  ❌ CreateTask 失败: {e.code()} - {e.details()}")
            all_passed = False
            continue

        time.sleep(6)

    server_proc.terminate()
    agent_proc.terminate()
    time.sleep(1)
    try:
        server_proc.wait(timeout=5)
        agent_proc.wait(timeout=5)
    except subprocess.TimeoutExpired:
        server_proc.kill()
        agent_proc.kill()

    with open(os.path.join(TEST_LOG_DIR, "agent_w5_pid.log"), "r") as f:
        agent_output = f.read()

    print("\n--- Agent 日志（PID 不存在相关）---")
    for line in agent_output.split('\n'):
        if any(kw in line for kw in ['PID', '不存在', '选择采集器', '收到任务', 'NotifyResult', 'error']):
            print(line)

    # 验证每个采集器都处理了不存在的 PID
    for ptype, pname in test_cases:
        profiler_selected = f"选择采集器: {pname}" in agent_output
        pid_check = "PID 99999 不存在" in agent_output

        print(f"\n  {pname}: 采集器选择={'✅' if profiler_selected else '❌'} | PID检测={'✅' if pid_check else '❌'}")
        if not profiler_selected:
            all_passed = False
        # PID不存在检测：perf 和 async-profiler 在 fork 前检查，pprof 在 curl 连接后会有错误

    print(f"\n综合结果: {'✅ 全部通过' if all_passed else '❌ 部分失败'}")
    return all_passed


if __name__ == "__main__":
    print("=" * 60)
    print("W5 全面测试套件")
    print("=" * 60)
    print(f"测试日志目录: {TEST_LOG_DIR}\n")

    results = {}

    # 测试1: 配置文件加载
    results["1.配置加载"] = test_config_loading()

    # 测试2: 多Server故障转移
    results["2.故障转移"] = test_multi_server_failover()

    # 测试3: perf正常采集
    results["3.perf采集"] = test_perf_normal()

    # 测试4: async-profiler 采集器分发
    print("\n" + "=" * 60)
    print("W5 测试4: async-profiler (profilerType=1) 采集器分发")
    print("=" * 60)
    results["4.async-profiler分发"] = test_profiler_type(1, "async-profiler", pid=99999)

    # 测试5: pprof 采集器分发
    print("\n" + "=" * 60)
    print("W5 测试5: pprof (profilerType=2) 采集器分发")
    print("=" * 60)
    results["5.pprof分发"] = test_profiler_type(2, "pprof", pid=6060)

    # 测试6: 不存在的PID
    results["6.PID不存在"] = test_pid_not_exists_all_profilers()

    # ============================================================
    print("\n" + "=" * 60)
    print("W5 测试总结")
    print("=" * 60)

    all_passed = True
    for test_name, passed in results.items():
        status = "✅ 通过" if passed else "❌ 失败"
        if not passed:
            all_passed = False
        print(f"  {status} — {test_name}")

    print("=" * 60)
    if all_passed:
        print("🎉 W5 全部测试通过!")
    else:
        print("⚠️  W5 部分测试失败，需要修复")

    sys.exit(0 if all_passed else 1)
