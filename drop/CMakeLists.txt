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

set(CMAKE_CXX_STANDARD 14)
set(CMAKE_CXX_STANDARD_REQUIRED ON)

# ---------- 依赖 ----------
find_package(Protobuf REQUIRED)
find_package(Threads REQUIRED)
find_package(PkgConfig REQUIRED)
pkg_check_modules(gRPC REQUIRED IMPORTED_TARGET grpc++ grpc)
set(gRPC_CPP_PLUGIN /usr/bin/grpc_cpp_plugin)

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
set(COMMON_SRCS
    common/Perf.cpp
    common/Process.cpp
    common/COSClient.cpp
    common/Utils.cpp
)

# ---------- server 模块源文件 ----------
set(SERVER_SRCS
    server/main.cpp
    server/AgentInfo.cpp
    server/TaskQueue.cpp
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
