# ============================================================
# analysis (Python) Dockerfile 小白版注释
# ============================================================
# Python 项目相对简单：装好 Python + 依赖包即可
# ============================================================

# 基于官方 Python 3.10 精简镜像
FROM python:3.10-slim

# 安装系统级工具：
#   perl            = 火焰图生成脚本 flamegraph.pl 是 Perl 写的
#   procps          = ps 等进程工具
#   linux-perf      = perf script 解析 perf.data，生成 CPU 火焰图
# 优先用腾讯云内网镜像加速 apt（腾讯云环境下更快）；mirrors.tencentyun.com
# 只有腾讯云内网机器能解析，非腾讯云环境 apt-get update 会直接挂掉。这里不
# 覆盖原有腾讯云配置，先按原样试一次，解析不到再回退到阿里云公网镜像重试。
# debian.sources 是新格式路径、sources.list 是旧格式，两个都尝试替换。
RUN (sed -i s@/deb.debian.org/@/mirrors.tencentyun.com/@g /etc/apt/sources.list.d/debian.sources 2>/dev/null || \
     sed -i s@/deb.debian.org/@/mirrors.tencentyun.com/@g /etc/apt/sources.list 2>/dev/null || true) && \
    (apt-get update -qq || \
     (sed -i s@/mirrors.tencentyun.com/@/mirrors.aliyun.com/@g /etc/apt/sources.list.d/debian.sources 2>/dev/null; \
      sed -i s@/mirrors.tencentyun.com/@/mirrors.aliyun.com/@g /etc/apt/sources.list 2>/dev/null; \
      apt-get update -qq))
RUN apt-get install -y --no-install-recommends \
    perl \
    procps \
    linux-perf \
    golang-go \
    graphviz \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# 先复制依赖清单，利用 Docker 缓存
COPY requirements.txt .
# 同样先试腾讯云内网 pypi 镜像，不可达再回退阿里云公网 pypi 镜像，不覆盖
# 原有腾讯云配置意图。
RUN pip install --no-cache-dir -i http://mirrors.tencentyun.com/pypi/simple --trusted-host mirrors.tencentyun.com -r requirements.txt || \
    pip install --no-cache-dir -i https://mirrors.aliyun.com/pypi/simple/ --trusted-host mirrors.aliyun.com -r requirements.txt

# 复制所有分析脚本
COPY . .

# 容器启动时默认执行分析入口脚本
ENTRYPOINT ["python3", "hotmethod_analyzer.py"]
