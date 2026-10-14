const config = require('./config.js');
const api = require('./utils/api.js');

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
  },
});
