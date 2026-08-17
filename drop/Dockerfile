# ============================================================
# drop (C++) Dockerfile 小白版注释
# ============================================================
# C++ 项目编译慢、依赖重，所以分两个阶段：
#   阶段1：在完整的 Ubuntu 里编译（有编译器、开发库）
#   阶段2：只复制编译好的二进制文件到干净环境
# ============================================================

# ---------- 阶段1：编译 ----------
FROM ubuntu:22.04 AS builder

# 腾讯云内网镜像加速 apt（服务器上构建更快，其他网络环境 sed 失败不影响）
RUN sed -i s@/archive.ubuntu.com/@/mirrors.tencentyun.com/@g /etc/apt/sources.list && sed -i s@/security.ubuntu.com/@/mirrors.tencentyun.com/@g /etc/apt/sources.list

# 禁止 apt 安装时弹出交互式界面（Docker 里没有终端）
ENV DEBIAN_FRONTEND=noninteractive

# 安装编译 C++ 代码需要的工具：
#   build-essential  = gcc/g++/make 等基础编译工具
#   cmake            = 构建系统（类似 Makefile 生成器）
#   protobuf-compiler = protoc，把 .proto 文件编译成 C++ 代码
#   libprotobuf-dev   = protobuf 的 C++ 开发库
#   libgrpc++-dev     = gRPC 的 C++ 开发库（含 grpc_cpp_plugin）
#   libgtest-dev      = gtest 单元测试框架（可选，drop_tests 目标用；
#                        找不到也不影响 drop_server/drop_agent 的构建）
RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential \
    cmake \
    pkg-config \
    protobuf-compiler \
    protobuf-compiler-grpc \
    libprotobuf-dev \
    libgrpc++-dev \
    libgrpc-dev \
    libgtest-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY . .

# CMake 构建（先清理本地残留的 CMakeCache，避免路径冲突）
RUN rm -rf build && mkdir -p build && cd build && cmake .. && make -j$(nproc)

# ---------- 阶段2：运行 ----------
FROM ubuntu:22.04

ENV DEBIAN_FRONTEND=noninteractive

# 只安装"运行时"需要的库（不需要编译器了，镜像更小）
#   libgrpc++1      = gRPC C++ 运行时库
#   libprotobuf23   = protobuf 运行时库
#   linux-tools-generic = perf 工具（采集性能需要）
#   bpftrace        = eBPF 内核态探针（profilerType=3）
#   perl            = 火焰图脚本需要
#   curl            = 健康检查 + MinIO API 调用
#   wget            = 下载 mc (MinIO Client)
RUN apt-get update && apt-get install -y --no-install-recommends \
    libgrpc++1 \
    libprotobuf23 \
    linux-tools-generic \
    linux-headers-generic \
    linux-libc-dev \
    bpftrace \
    perl \
    curl \
    wget \
    ca-certificates \
    && perf_real="$(find /usr/lib -type f -path '*/linux-tools*/perf' | sort -V | tail -n 1)" \
    && test -n "$perf_real" \
    && ln -sf "$perf_real" /usr/local/bin/perf-real \
    && rm -rf /var/lib/apt-lists/*

# Pin async-profiler so Java collection is available in every Agent image.
# The launcher is placed on PATH and emits collapsed stacks in the Agent code.
ARG ASYNC_PROFILER_VERSION=4.4
RUN curl -fsSL "https://github.com/async-profiler/async-profiler/releases/download/v${ASYNC_PROFILER_VERSION}/async-profiler-${ASYNC_PROFILER_VERSION}-linux-x64.tar.gz" \
    | tar -xz -C /opt \
    && ln -s "/opt/async-profiler-${ASYNC_PROFILER_VERSION}-linux-x64/bin/asprof" /usr/local/bin/asprof \
    && asprof --version

# 安装 MinIO Client (mc)，用于上传采集结果到 MinIO
RUN wget -q https://dl.min.io/client/mc/release/linux-amd64/mc -O /usr/local/bin/mc \
    && chmod +x /usr/local/bin/mc

WORKDIR /app
# 从阶段1复制编译好的两个程序
COPY --from=builder /app/build/drop_server .
COPY --from=builder /app/build/drop_agent .

# perf 采集需要特权模式（在 docker-compose 里用 privileged: true）
EXPOSE 50051
