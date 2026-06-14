# ============================================================
# CMakeLists.txt — C++ 项目构建配置 小白版注释
# ============================================================
# CMake 是 C++ 的"项目说明书"，告诉 cmake 怎么编译你的代码
# 相当于 Makefile 的生成器，但比手写 Makefile 简单很多
#
# CMake 语法小课堂：
#   cmake_minimum_required  = 最低 CMake 版本
#   project()               = 项目名和语言
#   set()                   = 定义变量
#   find_package()          = 查找已安装的库
#   add_executable()        = 生成一个可执行文件
#   target_link_libraries() = 链接库
# ============================================================

cmake_minimum_required(VERSION 3.16)   # 需要 CMake 3.16 以上
project(mini-drop VERSION 1.0.0 LANGUAGES CXX)  # 项目名 mini-drop，用 C++

# 使用 C++14 标准（14 = 2014 年发布的版本）
set(CMAKE_CXX_STANDARD 14)
set(CMAKE_CXX_STANDARD_REQUIRED ON)

# ---------- 查找依赖库 ----------
find_package(Protobuf REQUIRED)   # protobuf 库（必须安装）
find_package(gRPC CONFIG REQUIRED) # gRPC 库（必须安装）
find_package(Threads REQUIRED)     # 多线程库（系统自带）

# Proto 文件所在的目录
set(PROTO_DIR ${CMAKE_SOURCE_DIR}/common/proto)

# ---------- 定义 proto 生成的源文件列表 ----------
# 会在 ${CMAKE_BINARY_DIR}/gen/ 目录下生成这些文件
set(PROTO_SRCS
    ${CMAKE_BINARY_DIR}/gen/common.pb.cc
    ${CMAKE_BINARY_DIR}/gen/healthcheck.pb.cc
    ${CMAKE_BINARY_DIR}/gen/hotmethod.pb.cc
    ${CMAKE_BINARY_DIR}/gen/control.pb.cc
    ${CMAKE_BINARY_DIR}/gen/init.pb.cc
)

# gRPC 相关的生成文件（多了 _grpc 后缀）
set(PROTO_GRPC_SRCS
    ${CMAKE_BINARY_DIR}/gen/healthcheck.grpc.pb.cc
    ${CMAKE_BINARY_DIR}/gen/hotmethod.grpc.pb.cc
    ${CMAKE_BINARY_DIR}/gen/control.grpc.pb.cc
    ${CMAKE_BINARY_DIR}/gen/init.grpc.pb.cc
)

# ---------- 自定义构建步骤：编译 .proto → .cc/.h ----------
add_custom_target(generate_proto ALL
    COMMAND mkdir -p ${CMAKE_BINARY_DIR}/gen
    # 每个 proto 文件都要跑一遍 protoc
    COMMAND ${Protobuf_PROTOC_EXECUTABLE}
        --proto_path=${CMAKE_SOURCE_DIR}
        --cpp_out=${CMAKE_BINARY_DIR}/gen
        ${PROTO_DIR}/common.proto
    COMMAND ${Protobuf_PROTOC_EXECUTABLE}
        --proto_path=${CMAKE_SOURCE_DIR}
        --cpp_out=${CMAKE_BINARY_DIR}/gen
        ${PROTO_DIR}/hotmethod.proto
    COMMAND ${Protobuf_PROTOC_EXECUTABLE}
        --proto_path=${CMAKE_SOURCE_DIR}
        --cpp_out=${CMAKE_BINARY_DIR}/gen
        --grpc_out=${CMAKE_BINARY_DIR}/gen
        --plugin=protoc-gen-grpc=${gRPC_CPP_PLUGIN}
        ${PROTO_DIR}/healthcheck.proto
    COMMAND ${Protobuf_PROTOC_EXECUTABLE}
        --proto_path=${CMAKE_SOURCE_DIR}
        --cpp_out=${CMAKE_BINARY_DIR}/gen
        --grpc_out=${CMAKE_BINARY_DIR}/gen
        --plugin=protoc-gen-grpc=${gRPC_CPP_PLUGIN}
        ${PROTO_DIR}/control.proto
    COMMAND ${Protobuf_PROTOC_EXECUTABLE}
        --proto_path=${CMAKE_SOURCE_DIR}
        --cpp_out=${CMAKE_BINARY_DIR}/gen
        --grpc_out=${CMAKE_BINARY_DIR}/gen
        --plugin=protoc-gen-grpc=${gRPC_CPP_PLUGIN}
        ${PROTO_DIR}/init.proto
    DEPENDS ${PROTO_DIR}/common.proto ${PROTO_DIR}/healthcheck.proto
            ${PROTO_DIR}/hotmethod.proto ${PROTO_DIR}/control.proto
            ${PROTO_DIR}/init.proto
)

# ---------- 编译 drop_agent ----------
add_executable(drop_agent           # 生成可执行文件 drop_agent
    agent/main.cpp                  # 源文件
    ${PROTO_SRCS} ${PROTO_GRPC_SRCS} # proto 生成的文件
)
# 头文件搜索路径
target_include_directories(drop_agent PRIVATE
    ${CMAKE_BINARY_DIR}/gen         # proto 生成的头文件
    ${CMAKE_SOURCE_DIR}             # 项目根目录
    ${Protobuf_INCLUDE_DIRS}        # protobuf 的安装路径
)
# 链接的库
target_link_libraries(drop_agent
    gRPC::grpc++                   # gRPC C++ 库
    ${Protobuf_LIBRARIES}           # protobuf 库
    Threads::Threads                # 线程库
)
add_dependencies(drop_agent generate_proto)  # 先编译 proto 再编译 agent

# ---------- 编译 drop_server ----------
add_executable(drop_server
    server/main.cpp
    ${PROTO_SRCS} ${PROTO_GRPC_SRCS}
)
target_include_directories(drop_server PRIVATE
    ${CMAKE_BINARY_DIR}/gen
    ${CMAKE_SOURCE_DIR}
    ${Protobuf_INCLUDE_DIRS}
)
target_link_libraries(drop_server
    gRPC::grpc++
    ${Protobuf_LIBRARIES}
    Threads::Threads
)
add_dependencies(drop_server generate_proto)
