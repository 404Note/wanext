const config = require('../config.js');

// 拼接相对地址：后端未配置 BASE_URL 时 proxy_url 是相对路径（Web 页同源可用），
// 小程序内必须拼成绝对地址才能加载。
function resolveURL(u) {
  if (!u) return '';
  if (/^https?:\/\//i.test(u)) return u;
  if (u.charAt(0) === '/') return config.baseURL.replace(/\/+$/, '') + u;
  return u;
}

// 统一请求封装：baseURL 一处配置，超时 100 秒（对齐服务端 90 秒上限并留余量）。
function request(path, method, data) {
  return new Promise((resolve, reject) => {
    wx.request({
      url: config.baseURL + path,
      method,
      data,
      header: {
        'Content-Type': 'application/json',
        'MP-Version': config.version,
      },
      timeout: 100000,
      success(res) {
        resolve(res);
      },
      fail(err) {
        reject({ code: 'network', err });
      },
    });
  });
}

// 探测服务可用性：失败不抛错，只返回布尔值。
function checkHealth() {
  return request('/api/health', 'GET', null)
    .then((res) => res.statusCode === 200 && res.data && res.data.ok === true)
    .catch(() => false);
}

// 提取内容：整段内容原样提交，提取逻辑全部复用服务端。
// 仅取标题、作者与媒体列表，其余字段不使用。
// medias 顺序即展示顺序，前端不得重排、不得按 kind 分组。
function extractContent(inputText) {
  return request('/api/parse', 'POST', { url: inputText }).then((res) => {
    if (res.statusCode !== 200) {
      throw { code: 'http', status: res.statusCode, data: res.data };
    }
    const data = res.data || {};
    const medias = Array.isArray(data.medias) ? data.medias : [];
    return {
      title: data.title || '',
      author: data.author || '',
      medias: medias.map((m) => ({
        kind: m.kind,
        proxyUrl: resolveURL(m.proxy_url),
        coverUrl: resolveURL(m.cover_url) || '',
        width: m.width || 0,
        height: m.height || 0,
      })),
    };
  });
}

// 取媒体文件大小（字节）：对代理地址发 HEAD 请求读 Content-Length。
// 失败时返回 -1，调用方静默处理（不展示大小）。
function getMediaSize(proxyUrl) {
  return new Promise((resolve) => {
    wx.request({
      url: proxyUrl,
      method: 'HEAD',
      success(res) {
        const h = res.header || {};
        const len = h['content-length'] || h['Content-Length'];
        const n = parseInt(len, 10);
        resolve(Number.isFinite(n) && n >= 0 ? n : -1);
      },
      fail() {
        resolve(-1);
      },
    });
  });
}

function formatSize(bytes) {
  if (!Number.isFinite(bytes) || bytes < 0) return '';
  if (bytes < 1024) return bytes + ' B';
  if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(1) + ' KB';
  return (bytes / 1024 / 1024).toFixed(1) + ' MB';
}

module.exports = {
  request,
  checkHealth,
  extractContent,
  getMediaSize,
  formatSize,
};
