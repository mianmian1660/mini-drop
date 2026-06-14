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
RUN apt-get update && apt-get install -y --no-install-recommends \
    perl \
    procps \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# 先复制依赖清单，利用 Docker 缓存
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

# 复制所有分析脚本
COPY . .

# 容器启动时默认执行分析入口脚本
ENTRYPOINT ["python3", "hotmethod_analyzer.py"]
