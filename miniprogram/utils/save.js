// 保存链路：先获取临时文件，再存入相册。按 kind 选择对应的保存接口。
const auth = require('./auth.js');

function isAuthDeny(err) {
  const msg = (err && err.errMsg) || '';
  return msg.indexOf('auth deny') !== -1 || msg.indexOf('authorize') !== -1 || msg.indexOf('denied') !== -1;
}

function downloadTempFile(url, onProgress) {
  // 鉴权开启时顺带上令牌，进入后端的「带头则校验」规则。
  return auth.getToken().then(
    (token) =>
      new Promise((resolve, reject) => {
        const header = {};
        if (token) {
          header.Authorization = 'Bearer ' + token;
        }
        const task = wx.downloadFile({
          url,
          header,
          success(res) {
            if (res.statusCode === 200 && res.tempFilePath) {
              resolve(res.tempFilePath);
              return;
            }
            if (res.statusCode === 401) {
              // 令牌失效：清缓存，下次调用重新登录（本次按过期走已有重试链路）
              auth.invalidate();
            }
            reject({ code: res.statusCode === 404 ? 'expired' : 'downloadFailed', status: res.statusCode });
          },
          fail(err) {
            reject({ code: 'downloadFailed', err });
          },
        });
        if (typeof onProgress === 'function' && task && task.onProgressUpdate) {
          task.onProgressUpdate((res) => {
            onProgress(res.progress || 0);
          });
        }
      })
  );
}

function saveToAlbum(tempFilePath, kind) {
  if (kind === 'video') {
    return new Promise((resolve, reject) => {
      wx.saveVideoToPhotosAlbum({
        filePath: tempFilePath,
        success: resolve,
        fail: reject,
      });
    });
  }
  return new Promise((resolve, reject) => {
    wx.saveImageToPhotosAlbum({
      filePath: tempFilePath,
      success: resolve,
      fail: reject,
    });
  });
}

function saveMedia(proxyUrl, kind, onProgress) {
  return downloadTempFile(proxyUrl, onProgress).then((tempFilePath) => saveToAlbum(tempFilePath, kind));
}

module.exports = {
  isAuthDeny,
  saveMedia,
};
