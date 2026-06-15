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
find_package(Threads REQUIRED)     # 多线程库（系统自带）

# gRPC 用 pkg-config 查找（旧版系统 gRPC 不提供 cmake config）
find_package(PkgConfig REQUIRED)
pkg_check_modules(gRPC REQUIRED IMPORTED_TARGET grpc++ grpc)
# grpc_cpp_plugin 路径（protoc 编译 .proto 时需要）
set(gRPC_CPP_PLUGIN /usr/bin/grpc_cpp_plugin)

# Proto 文件所在的目录
set(PROTO_DIR ${CMAKE_SOURCE_DIR}/common/proto)
# proto 生成文件的输出目录
set(PROTO_GEN_DIR ${CMAKE_BINARY_DIR}/gen)

# ---------- 在 cmake 配置阶段就编译 .proto → .cc/.h ----------
# proto 文件内部 import 路径是 "common/proto/xxx.proto"，
# 所以生成的文件会放在 gen/common/proto/ 子目录下（保持目录结构）
execute_process(
    COMMAND mkdir -p ${PROTO_GEN_DIR}
)
foreach(PROTO_FILE
    common.proto
    hotmethod.proto
    healthcheck.proto
    control.proto
    init.proto
)
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

# ---------- 定义 proto 生成的源文件列表 ----------
# 生成的文件位于 gen/common/proto/ 下
set(PROTO_GEN_SUBDIR ${PROTO_GEN_DIR}/common/proto)
set(PROTO_SRCS
    ${PROTO_GEN_SUBDIR}/common.pb.cc
    ${PROTO_GEN_SUBDIR}/healthcheck.pb.cc
    ${PROTO_GEN_SUBDIR}/hotmethod.pb.cc
    ${PROTO_GEN_SUBDIR}/control.pb.cc
    ${PROTO_GEN_SUBDIR}/init.pb.cc
)

# gRPC 相关的生成文件（多了 _grpc 后缀）
set(PROTO_GRPC_SRCS
    ${PROTO_GEN_SUBDIR}/healthcheck.grpc.pb.cc
    ${PROTO_GEN_SUBDIR}/hotmethod.grpc.pb.cc
    ${PROTO_GEN_SUBDIR}/control.grpc.pb.cc
    ${PROTO_GEN_SUBDIR}/init.grpc.pb.cc
)

# ---------- 编译 drop_agent ----------
add_executable(drop_agent           # 生成可执行文件 drop_agent
    agent/main.cpp                  # 源文件
    ${PROTO_SRCS} ${PROTO_GRPC_SRCS} # proto 生成的文件
)
# 头文件搜索路径
target_include_directories(drop_agent PRIVATE
    ${PROTO_GEN_DIR}                # proto 生成的头文件
    ${CMAKE_SOURCE_DIR}             # 项目根目录
    ${Protobuf_INCLUDE_DIRS}        # protobuf 的安装路径
    ${gRPC_INCLUDE_DIRS}            # gRPC 头文件路径（来自 pkg-config）
)
# 链接的库
target_link_libraries(drop_agent
    PkgConfig::gRPC                 # gRPC 库（pkg-config 导入的目标）
    ${Protobuf_LIBRARIES}           # protobuf 库
    Threads::Threads                # 线程库
)

# ---------- 编译 drop_server ----------
add_executable(drop_server
    server/main.cpp
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
)
