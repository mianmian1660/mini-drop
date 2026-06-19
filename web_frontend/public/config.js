// ============================================================
// config.js — 运行时配置（在 index.html 之前加载）
// ============================================================
// 生产部署时，由 nginx 模板或环境变量注入真实值
// 本地开发时，HOST_URL 留空 = 同源请求，由 React dev server proxy 转发
//
// 例如 nginx 可配置为：
//   sub_filter 'window.config = {' 'window.config = { HOST_URL: "http://apiserver:8191",';
// ============================================================
window.config = {
    HOST_URL: '',   // 空 = 同源；生产环境填写 "http://apiserver:8191"
};
