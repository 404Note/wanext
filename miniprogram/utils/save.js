// 保存链路：先获取临时文件，再存入相册。按 kind 选择对应的保存接口。

function isAuthDeny(err) {
  const msg = (err && err.errMsg) || '';
  return msg.indexOf('auth deny') !== -1 || msg.indexOf('authorize') !== -1 || msg.indexOf('denied') !== -1;
}

function downloadTempFile(url, onProgress) {
  return new Promise((resolve, reject) => {
    const task = wx.downloadFile({
      url,
      success(res) {
        if (res.statusCode === 200 && res.tempFilePath) {
          resolve(res.tempFilePath);
          return;
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
  });
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
