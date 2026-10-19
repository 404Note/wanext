const config = require('./config.js');
const api = require('./utils/api.js');
const auth = require('./utils/auth.js');

App({
  globalData: {
    baseURL: config.baseURL,
    version: config.version,
    serviceAvailable: false,
  },

  onLaunch() {
    api
      .checkHealth()
      .then((ok) => {
        this.globalData.serviceAvailable = ok;
      })
      .catch(() => {
        this.globalData.serviceAvailable = false;
      });
    // 预热登录：让用户点「开始提取」时不再串行等登录；失败静默（提取时还会再试）。
    auth.getToken().catch(() => {});
  },
});
