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

# 利用 Docker 缓存层：先复制依赖清单
COPY package.json ./
RUN npm install --production=false

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
    CMD wget -qO- http://localhost/health || exit 1

EXPOSE 80

CMD ["nginx", "-g", "daemon off;"]
