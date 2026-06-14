# ============================================================
# web_frontend (React) Dockerfile 小白版注释
# ============================================================
# React 项目也分两阶段：
#   阶段1：用 Node.js 编译 React 代码 → 生成静态 HTML/JS/CSS
#   阶段2：用 Nginx 托管这些静态文件（高性能 Web 服务器）
# ============================================================

# ---------- 阶段1：编译 React ----------
FROM node:18-alpine AS builder

WORKDIR /app

# 复制 package.json（依赖清单）
COPY package.json ./
# npm install：根据 package.json 安装所有依赖
RUN npm install

# 复制源码 → 编译
COPY public/ public/
COPY src/ src/
# npm run build：React 编译命令，输出到 build/ 目录
RUN npm run build

# ---------- 阶段2：Nginx 托管 ----------
FROM nginx:alpine

# 把编译好的静态文件复制到 Nginx 的网页目录
COPY --from=builder /app/build /usr/share/nginx/html
# 用我们自己的 nginx 配置覆盖默认配置
COPY nginx.conf /etc/nginx/conf.d/default.conf

EXPOSE 80
# 启动 Nginx（-g "daemon off;" 让 Nginx 在前台运行，容器不会退出）
CMD ["nginx", "-g", "daemon off;"]
