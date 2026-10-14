// 读取粘贴板内容：必须由用户点击触发，不做后台静默读取。
function getClipboardText() {
  return new Promise((resolve, reject) => {
    wx.getClipboardData({
      success(res) {
        resolve(res.data || '');
      },
      fail(err) {
        reject(err);
      },
    });
  });
}

module.exports = {
  getClipboardText,
};
