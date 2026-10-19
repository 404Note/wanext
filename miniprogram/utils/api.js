const config = require('../config.js');

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
        proxyUrl: m.proxy_url,
        directUrl: m.direct_url,
        coverUrl: m.cover_url || '',
        width: m.width || 0,
        height: m.height || 0,
      })),
    };
  });
}

module.exports = {
  request,
  checkHealth,
  extractContent,
};
