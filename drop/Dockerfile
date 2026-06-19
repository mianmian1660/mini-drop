# ============================================================
# drop (C++) Dockerfile 小白版注释
# ============================================================
# C++ 项目编译慢、依赖重，所以分两个阶段：
#   阶段1：在完整的 Ubuntu 里编译（有编译器、开发库）
#   阶段2：只复制编译好的二进制文件到干净环境
# ============================================================

# ---------- 阶段1：编译 ----------
FROM ubuntu:22.04 AS builder

# 禁止 apt 安装时弹出交互式界面（Docker 里没有终端）
ENV DEBIAN_FRONTEND=noninteractive

# 安装编译 C++ 代码需要的工具：
#   build-essential  = gcc/g++/make 等基础编译工具
#   cmake            = 构建系统（类似 Makefile 生成器）
#   protobuf-compiler = protoc，把 .proto 文件编译成 C++ 代码
#   libprotobuf-dev   = protobuf 的 C++ 开发库
#   libgrpc++-dev     = gRPC 的 C++ 开发库（含 grpc_cpp_plugin）
RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential \
    cmake \
    pkg-config \
    protobuf-compiler \
    protobuf-compiler-grpc \
    libprotobuf-dev \
    libgrpc++-dev \
    libgrpc-dev \
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
#   perl            = 火焰图脚本需要
#   curl            = 健康检查 + MinIO API 调用
#   wget            = 下载 mc (MinIO Client)
RUN apt-get update && apt-get install -y --no-install-recommends \
    libgrpc++1 \
    libprotobuf23 \
    linux-tools-generic \
    perl \
    curl \
    wget \
    ca-certificates \
    && rm -rf /var/lib/apt-lists/*

# 安装 MinIO Client (mc)，用于上传采集结果到 MinIO
RUN wget -q https://dl.min.io/client/mc/release/linux-amd64/mc -O /usr/local/bin/mc \
    && chmod +x /usr/local/bin/mc

WORKDIR /app
# 从阶段1复制编译好的两个程序
COPY --from=builder /app/build/drop_server .
COPY --from=builder /app/build/drop_agent .

# perf 采集需要特权模式（在 docker-compose 里用 privileged: true）
EXPOSE 50051
