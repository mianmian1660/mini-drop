# ============================================================
# apiserver Dockerfile 小白版注释
# ============================================================
# 这个文件告诉 Docker 如何把 Go 代码打包成一个可运行的容器
# 分两个阶段：阶段1编译 → 阶段2运行（最终镜像很小）
# ============================================================

# ---------- 阶段1：编译 ----------
# FROM：以哪个镜像为基础。这里用装了 Go 1.19 的精简 Linux
# AS builder：起个名字叫 builder，后面可以引用
FROM golang:1.19-alpine AS builder

# WORKDIR：设定工作目录（相当于 cd）
WORKDIR /app

# 先复制依赖清单，利用 Docker 缓存（依赖没变就不用重新下载）
COPY go.mod go.sum ./
RUN go mod download

# 复制所有源码，然后编译
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o apiserver .

# ---------- 阶段2：运行 ----------
# 换一个超小的基础镜像（~5MB），只放编译好的程序
FROM alpine:3.18

# 安装运行必需的 SSL 证书和时区数据
RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app
# --from=builder：从阶段1的镜像里拿编译好的二进制文件
COPY --from=builder /app/apiserver .
# 复制配置文件（Docker 内使用环境变量覆盖关键参数）
COPY --from=builder /app/apiserver.yaml ./apiserver.yaml

# 文档说明：这个容器用 8191 端口
EXPOSE 8191
# 启动时运行 ./apiserver
ENTRYPOINT ["./apiserver"]
