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
ENV GOPROXY=https://goproxy.cn,direct
# 注：这里原来有一行替换 apk 源的 sed，实际是死代码——本阶段(builder)从没
# 执行过 apk add，真正用到 apk 的是下面阶段2(FROM alpine:3.18)，两个阶段
# 是独立镜像，这里改的源对阶段2毫无影响。阶段2走的是 alpine 默认公网源，
# 本来就能连，不需要额外替换，删掉这行死代码。

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

# apiserver is a static Go binary and only talks to in-cluster services over
# HTTP. Avoid a runtime apk download: deployment must remain possible when the
# host is under disk/network pressure, exactly when retention recovery matters.

WORKDIR /app
# --from=builder：从阶段1的镜像里拿编译好的二进制文件
COPY --from=builder /app/apiserver .
# 复制配置文件（Docker 内使用环境变量覆盖关键参数）
COPY --from=builder /app/apiserver.yaml ./apiserver.yaml

# 文档说明：这个容器用 8191 端口
EXPOSE 8191
# 启动时运行 ./apiserver
ENTRYPOINT ["./apiserver"]
