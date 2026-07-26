const { createProxyMiddleware } = require('http-proxy-middleware');

module.exports = function configureProxy(app) {
    app.use(
        createProxyMiddleware('/api', {
            target: 'http://localhost:8191',
            changeOrigin: true,
        }),
    );
};
