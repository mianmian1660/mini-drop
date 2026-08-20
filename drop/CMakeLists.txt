# ============================================================
# CMakeLists.txt — Mini-Drop C++ 项目构建配置
# ============================================================
# 模块拆分（按复刻指南 1.2 节）：
#   common/   — 共享库（Perf / Process / COSClient / Utils）
#   agent/    — drop_agent 入口
#   server/   — drop_server 入口 + 4 个 Service
# ============================================================

cmake_minimum_required(VERSION 3.16)
project(mini-drop VERSION 1.0.0 LANGUAGES CXX)

# 新版 grpc/absl 头文件依赖 C++17（本机 Homebrew grpc 1.83 实测）；
# 服务器 Ubuntu g++11 完全支持 C++17，兼容。
set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)

# ---------- 依赖 ----------
find_package(Protobuf REQUIRED)
find_package(Threads REQUIRED)
find_package(PkgConfig REQUIRED)
pkg_check_modules(gRPC REQUIRED IMPORTED_TARGET grpc++ grpc)
# 跨平台查找 grpc 的 protoc 插件：Ubuntu 在 /usr/bin，Homebrew(macOS) 在 /opt/homebrew/bin
find_program(gRPC_CPP_PLUGIN NAMES grpc_cpp_plugin REQUIRED)

# ---------- Proto 路径 ----------
set(PROTO_DIR ${CMAKE_SOURCE_DIR}/common/proto)
set(PROTO_GEN_DIR ${CMAKE_BINARY_DIR}/gen)

# ---------- 编译 .proto ----------
execute_process(COMMAND mkdir -p ${PROTO_GEN_DIR})
foreach(PROTO_FILE common.proto hotmethod.proto healthcheck.proto control.proto init.proto)
    execute_process(
        COMMAND ${Protobuf_PROTOC_EXECUTABLE}
            --proto_path=${CMAKE_SOURCE_DIR}
            --cpp_out=${PROTO_GEN_DIR}
            --grpc_out=${PROTO_GEN_DIR}
            --plugin=protoc-gen-grpc=${gRPC_CPP_PLUGIN}
            ${PROTO_DIR}/${PROTO_FILE}
        RESULT_VARIABLE PROTOC_RESULT
    )
    if(NOT PROTOC_RESULT EQUAL 0)
        message(FATAL_ERROR "protoc failed for ${PROTO_FILE}")
    endif()
endforeach()

# ---------- Proto 生成的文件列表 ----------
set(PROTO_GEN_SUBDIR ${PROTO_GEN_DIR}/common/proto)
set(PROTO_SRCS
    ${PROTO_GEN_SUBDIR}/common.pb.cc
    ${PROTO_GEN_SUBDIR}/healthcheck.pb.cc
    ${PROTO_GEN_SUBDIR}/hotmethod.pb.cc
    ${PROTO_GEN_SUBDIR}/control.pb.cc
    ${PROTO_GEN_SUBDIR}/init.pb.cc
)
set(PROTO_GRPC_SRCS
    ${PROTO_GEN_SUBDIR}/healthcheck.grpc.pb.cc
    ${PROTO_GEN_SUBDIR}/hotmethod.grpc.pb.cc
    ${PROTO_GEN_SUBDIR}/control.grpc.pb.cc
    ${PROTO_GEN_SUBDIR}/init.grpc.pb.cc
)

# ---------- common 模块源文件 ----------
# 注：perf/async-profiler/pprof/eBPF 的采集实现已经从 common/Perf.cpp 等
# 文件迁移到 agent/runners/*Runner.cpp（drop_agent::Runner 生命周期接口），
# 旧文件已删除，不再是 drop_server 也链接的"共享"代码。
set(COMMON_SRCS
    common/CapabilityDetector.cpp
    common/ContinuousSampler.cpp
    common/RuntimeSymbolMap.cpp
    common/GoSymbolizer.cpp
    common/Process.cpp
    common/COSClient.cpp
    common/Utils.cpp
    common/BuildId.cpp
    common/SymbolCollector.cpp
    common/Log.cpp
    common/ProcessExecutor.cpp
    common/ObjectStore.cpp
    common/LoggerIface.cpp
    common/PidRegistry.cpp
)

# ---------- server 模块源文件 ----------
set(SERVER_SRCS
    server/main.cpp
    server/AgentInfo.cpp
    server/TaskQueue.cpp
    server/ResultNotifier.cpp
    server/HealthCheckService.cpp
    server/HotmethodService.cpp
    server/ControlService.cpp
    server/InitAgentInfoService.cpp
)

# ---------- 查找 gRPC 反射库 ----------
find_library(gRPC_REFLECTION_LIB grpc++_reflection
    PATHS /usr/lib/x86_64-linux-gnu
)

# ============================================================
# 编译 drop_agent
# ============================================================
add_executable(drop_agent
    agent/main.cpp
    agent/Config.cpp
    agent/AgentUtils.cpp
    agent/TaskQueue.cpp
    agent/UploadQueue.cpp
    agent/AttemptTracker.cpp
    agent/ServerChannelHolder.cpp
    agent/HeartbeatThread.cpp
    agent/WorkerThread.cpp
    agent/UploadWorker.cpp
    agent/ResultDelivery.cpp
    agent/CleanupWorker.cpp
    agent/RunnerUtils.cpp
    agent/RunnerRegistry.cpp
    agent/runners/PerfRunner.cpp
    agent/runners/AsyncProfilerRunner.cpp
    agent/runners/PprofRunner.cpp
    agent/runners/BpfRunner.cpp
    ${COMMON_SRCS}
    ${PROTO_SRCS} ${PROTO_GRPC_SRCS}
)
target_include_directories(drop_agent PRIVATE
    ${PROTO_GEN_DIR}
    ${CMAKE_SOURCE_DIR}
    ${Protobuf_INCLUDE_DIRS}
    ${gRPC_INCLUDE_DIRS}
)
target_link_libraries(drop_agent
    PkgConfig::gRPC
    ${Protobuf_LIBRARIES}
    Threads::Threads
)

# ============================================================
# 编译 drop_server
# ============================================================
add_executable(drop_server
    ${SERVER_SRCS}
    ${COMMON_SRCS}
    ${PROTO_SRCS} ${PROTO_GRPC_SRCS}
)
target_include_directories(drop_server PRIVATE
    ${PROTO_GEN_DIR}
    ${CMAKE_SOURCE_DIR}
    ${Protobuf_INCLUDE_DIRS}
    ${gRPC_INCLUDE_DIRS}
)
target_link_libraries(drop_server
    PkgConfig::gRPC
    ${Protobuf_LIBRARIES}
    Threads::Threads
    ${gRPC_REFLECTION_LIB}
)

# ============================================================
# 单元测试：drop_tests（gtest，只覆盖不依赖真实进程/网络的纯逻辑）
# ============================================================
# 找不到 gtest 也不影响 drop_server/drop_agent 的正常构建（不用 REQUIRED）；
# EXCLUDE_FROM_ALL 让 `make`/`make -j$(nproc)`（不指定目标时的默认行为）
# 不会顺带编译测试目标，只有显式 `cmake --build . --target drop_tests`
# 才会碰到它——测试代码的任何问题都不会拖累主构建/部署。
find_package(GTest)
if(GTest_FOUND)
    add_executable(drop_tests EXCLUDE_FROM_ALL
        tests/test_task_queue.cpp
        tests/test_process_executor.cpp
        tests/test_runners.cpp
        tests/test_agent_task_queue.cpp
        tests/test_attempt_tracker.cpp
        tests/test_upload_queue.cpp
        tests/test_result_delivery.cpp
        tests/test_agent_utils.cpp
        tests/test_pid_registry.cpp
        tests/test_runtime_symbol_map.cpp
        tests/test_go_symbolizer.cpp
        tests/test_continuous_sampler.cpp
        server/TaskQueue.cpp
        common/ProcessExecutor.cpp
        common/Process.cpp
        common/Utils.cpp
        common/BuildId.cpp
        common/PidRegistry.cpp
        common/RuntimeSymbolMap.cpp
        common/GoSymbolizer.cpp
        agent/RunnerUtils.cpp
        agent/TaskQueue.cpp
        agent/UploadQueue.cpp
        agent/AttemptTracker.cpp
        agent/ResultDelivery.cpp
        agent/AgentUtils.cpp
        agent/runners/PerfRunner.cpp
        agent/runners/AsyncProfilerRunner.cpp
        agent/runners/PprofRunner.cpp
        agent/runners/BpfRunner.cpp
        ${PROTO_SRCS}
    )
    target_include_directories(drop_tests PRIVATE
        ${PROTO_GEN_DIR}
        ${CMAKE_SOURCE_DIR}
        ${Protobuf_INCLUDE_DIRS}
    )
    target_link_libraries(drop_tests
        GTest::gtest
        GTest::gtest_main
        ${Protobuf_LIBRARIES}
        Threads::Threads
    )
    enable_testing()
    add_test(NAME drop_tests COMMAND drop_tests)
else()
    message(STATUS "GTest 未找到，跳过 drop_tests 目标（不影响 drop_server/drop_agent 构建）")
endif()
