# ============================================================
# web_frontend (React) Dockerfile — W5 生产版
# ============================================================
# 两阶段构建：
#   阶段1：Node.js 编译 React → 静态 HTML/JS/CSS
#   阶段2：Nginx 托管静态文件（高性能 Web 服务器）
# ============================================================

# ---------- 阶段1：编译 React ----------
FROM node:18-alpine AS builder

WORKDIR /app

# 利用 Docker 缓存层：先复制依赖清单，源码放在后面，改源码不会重装依赖
#
# 依赖安装是整个构建里最慢的一步（react-scripts 会拖进 1000+ 个传递依赖），
# 这里做了两件事把它从 20 分钟级压到分钟级：
#   1. 连 package-lock.json 一起复制。缺了它 npm 必须联网逐个查询元数据来解析
#      版本范围；有它就直接按锁定版本下载，同时保证构建可复现。
#   2. 默认走国内镜像源。境外构建可以覆盖回官方源：
#      docker compose build --build-arg NPM_REGISTRY=https://registry.npmjs.org
ARG NPM_REGISTRY=https://registry.npmmirror.com
COPY package.json package-lock.json ./
RUN npm config set registry "$NPM_REGISTRY" \
    && npm install --include=dev

# 复制源码 → 编译
COPY public/ public/
COPY src/ src/
RUN npm run build

# ---------- 阶段2：Nginx 托管 ----------
FROM nginx:alpine

# 把编译产物复制到 Nginx 网页目录
COPY --from=builder /app/build /usr/share/nginx/html

# 用自定义 nginx 配置覆盖默认配置
COPY nginx.conf /etc/nginx/conf.d/default.conf

# W5: Docker 健康检查（每 30 秒探测 /health 端点）
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1/health || exit 1

EXPOSE 80

CMD ["nginx", "-g", "daemon off;"]
