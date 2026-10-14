// 全局配置：真实值只放 config.local.js（未追踪，绝不提交）；此处仅为占位回退。
// 本地开发可用 http://127.0.0.1:8080（开发者工具需勾选不校验合法域名）。
let local = {};
try {
  local = require('./config.local.js');
} catch (_) {
  local = {};
}

const config = {
  baseURL: local.baseURL || 'https://api.example.com',
  version: '0.1.0',
};

module.exports = config;
