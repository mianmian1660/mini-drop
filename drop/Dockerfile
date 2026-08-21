# ============================================================
# drop (C++) Dockerfile 小白版注释
# ============================================================
# C++ 项目编译慢、依赖重，所以分两个阶段：
#   阶段1：在完整的 Ubuntu 里编译（有编译器、开发库）
#   阶段2：只复制编译好的二进制文件到干净环境
# ============================================================

# Reuse the already-published client image instead of following a large GitHub
# redirect during every Agent runtime build.
FROM minio/mc@sha256:a7fe349ef4bd8521fb8497f55c6042871b2ae640607cf99d9bede5e9bdf11727 AS minio-client

# ---------- 阶段1：编译 ----------
FROM ubuntu:22.04 AS builder

# 优先用腾讯云内网镜像加速 apt（腾讯云环境下更快）；mirrors.tencentyun.com
# 只有腾讯云内网机器能解析，非腾讯云环境会挂掉。这里不覆盖原有腾讯云配置，
# 先按原样替换，用 getent hosts 显式探测能不能解析，不能就回退阿里云公网
# 镜像——不能靠 apt-get update 自己的退出码判断，它遇到源不可达时默认只是
# 打 warning、照样返回 0（真正的硬失败要等后面 apt-get install 才会暴露）。
RUN sed -i s@/archive.ubuntu.com/@/mirrors.tencentyun.com/@g /etc/apt/sources.list && \
    sed -i s@/security.ubuntu.com/@/mirrors.tencentyun.com/@g /etc/apt/sources.list && \
    if ! getent hosts mirrors.tencentyun.com > /dev/null 2>&1; then \
        sed -i s@/mirrors.tencentyun.com/@/mirrors.aliyun.com/@g /etc/apt/sources.list; \
    fi

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
    libbpf-dev \
    clang \
    nlohmann-json3-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY . .

# CMake 构建（先清理本地残留的 CMakeCache，避免路径冲突）
ARG DROP_BUILD_JOBS=2
RUN rm -rf build && mkdir -p build && cd build && cmake .. && make -j"${DROP_BUILD_JOBS}"

# Explicit test target for CI/local verification. The production runtime stage
# does not depend on this stage, so normal image builds remain unchanged.
FROM builder AS test
ARG DROP_BUILD_JOBS=2
RUN cmake --build build --target drop_tests -j"${DROP_BUILD_JOBS}" \
    && ctest --test-dir build --output-on-failure

# ---------- 阶段2：运行 ----------
FROM ubuntu:22.04

ENV DEBIAN_FRONTEND=noninteractive

# Runtime is a fresh stage, so repeat the builder's reachable mirror selection.
RUN sed -i s@/archive.ubuntu.com/@/mirrors.tencentyun.com/@g /etc/apt/sources.list && \
    sed -i s@/security.ubuntu.com/@/mirrors.tencentyun.com/@g /etc/apt/sources.list && \
    if ! getent hosts mirrors.tencentyun.com > /dev/null 2>&1; then \
        sed -i s@/mirrors.tencentyun.com/@/mirrors.aliyun.com/@g /etc/apt/sources.list; \
    fi

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
    libbpf0 \
    perl \
    curl \
    unzip \
    python3 \
    python3-pip \
    ca-certificates \
    && perf_real="$(find /usr/lib -type f -path '*/linux-tools*/perf' | sort -V | tail -n 1)" \
    && test -n "$perf_real" \
    && ln -sf "$perf_real" /usr/local/bin/perf-real \
    && rm -rf /var/lib/apt-lists/*

# Recover function ranges from stripped Go binaries. Pin and verify the release
# artifact so profiling behavior cannot change underneath an image rebuild.
ARG GORESYM_VERSION=3.4
ARG GORESYM_ZIP_SHA256=cdcbda1273de4a502044520af3cbdb73149e74847aa1c515b413de4243e972f4
RUN (curl --max-time 30 -fsSL "https://github.com/mandiant/GoReSym/releases/download/v${GORESYM_VERSION}/GoReSym-linux.zip" -o /tmp/goresym.zip \
      || curl --max-time 60 -fsSL "https://ghfast.top/https://github.com/mandiant/GoReSym/releases/download/v${GORESYM_VERSION}/GoReSym-linux.zip" -o /tmp/goresym.zip) \
    && echo "${GORESYM_ZIP_SHA256}  /tmp/goresym.zip" | sha256sum -c - \
    && unzip -p /tmp/goresym.zip GoReSym > /usr/local/bin/GoReSym \
    && chmod 0755 /usr/local/bin/GoReSym \
    && rm -f /tmp/goresym.zip

# Pin async-profiler so Java collection is available in every Agent image.
# The launcher is placed on PATH and emits collapsed stacks in the Agent code.
ARG ASYNC_PROFILER_VERSION=4.4
RUN (curl --max-time 30 -fsSL "https://github.com/async-profiler/async-profiler/releases/download/v${ASYNC_PROFILER_VERSION}/async-profiler-${ASYNC_PROFILER_VERSION}-linux-x64.tar.gz" -o /tmp/async-profiler.tgz \
      || curl --max-time 60 -fsSL "https://ghfast.top/https://github.com/async-profiler/async-profiler/releases/download/v${ASYNC_PROFILER_VERSION}/async-profiler-${ASYNC_PROFILER_VERSION}-linux-x64.tar.gz" -o /tmp/async-profiler.tgz) \
    && tar -xzf /tmp/async-profiler.tgz -C /opt \
    && rm -f /tmp/async-profiler.tgz \
    && ln -s "/opt/async-profiler-${ASYNC_PROFILER_VERSION}-linux-x64/bin/asprof" /usr/local/bin/asprof \
    && asprof --version

# Pure-Python stack fallback for CPython processes without perf maps. The
# wheel is pinned by digest and only its standalone Rust binary is installed.
ARG PYSPY_VERSION=0.4.2
ARG PYSPY_WHEEL_SHA256=aeb0323409199c785f730645e9f4bb7a7b9ca2c481f2c331a55642b5d13fa52f
ARG PYSPY_WHEEL_URL=https://files.pythonhosted.org/packages/f9/34/dd7d3c763a00b7b965e25a5eab0acd1a345dbaf0f45fffe595278873a1c0/py_spy-0.4.2-py2.py3-none-manylinux_2_5_x86_64.manylinux1_x86_64.whl
RUN curl -fsSL "${PYSPY_WHEEL_URL}" -o /tmp/py-spy.whl \
    && echo "${PYSPY_WHEEL_SHA256}  /tmp/py-spy.whl" | sha256sum -c - \
    && unzip -q /tmp/py-spy.whl -d /tmp/py-spy-wheel \
    && pyspy_bin="$(find /tmp/py-spy-wheel -type f -name py-spy | head -n 1)" \
    && test -n "${pyspy_bin}" \
    && install -m 0755 "${pyspy_bin}" /usr/local/bin/py-spy \
    && py-spy --version | grep -F "py-spy ${PYSPY_VERSION}" \
    && rm -rf /tmp/py-spy.whl /tmp/py-spy-wheel

ARG MEMRAY_VERSION=1.20.0
RUN python3 -m pip install --no-cache-dir -i https://mirrors.cloud.tencent.com/pypi/simple "memray==${MEMRAY_VERSION}" \
    && python3 -c "import memray; assert memray.__version__ == '${MEMRAY_VERSION}'"

# 安装 MinIO Client (mc)，用于上传采集结果到 MinIO。
COPY --from=minio-client /usr/bin/mc /usr/local/bin/mc

WORKDIR /app
# 从阶段1复制编译好的两个程序
COPY --from=builder /app/build/drop_server .
COPY --from=builder /app/build/drop_agent .
COPY --from=builder /app/build/native_cp.bpf.o /app/native_cp.bpf.o
COPY scripts/memray_converter.py /app/memray_converter.py

# perf 采集需要特权模式（在 docker-compose 里用 privileged: true）
EXPOSE 50051
