// 令牌管理：取、存、判过期、单飞登录。
//
// 设计要点（见《wx-mp API 认证授权设计文档》第 9 节）：
//   - 令牌只存内存与本地缓存，不打 console.log、不进任何上报路径；
//   - 单飞（single-flight）：并发调用共享同一个登录 Promise，避免 code 互相作废；
//   - 登录不可用（开关关闭的开发环境 / 网络异常）时返回空串，调用方按无鉴权
//     模式继续——开关关闭时后端直通，不带令牌也能工作。
const config = require('../config.js');

const TOKEN_KEY = 'wanext_token';
const EXP_KEY = 'wanext_token_exp';
// 提前 60 秒视为过期，避开边界竞态。
const GRACE_MS = 60 * 1000;
// 登录不可用时 5 分钟内不再尝试，避免每个请求都走一次 wx.login。
const NOAUTH_RETRY_MS = 5 * 60 * 1000;

let memToken = '';
let memExp = 0;
let noAuthUntil = 0;
let loginPromise = null;

function loadCache() {
  if (memToken) return;
  try {
    const t = wx.getStorageSync(TOKEN_KEY);
    const e = wx.getStorageSync(EXP_KEY) || 0;
    if (t && e > Date.now() + GRACE_MS) {
      memToken = t;
      memExp = e;
    }
  } catch (_) {
    // 忽略存储异常
  }
}

function saveCache(token, expiresInSec) {
  memToken = token;
  memExp = Date.now() + (expiresInSec || 7200) * 1000;
  try {
    wx.setStorageSync(TOKEN_KEY, token);
    wx.setStorageSync(EXP_KEY, memExp);
  } catch (_) {
    // 忽略存储异常
  }
}

function clearCache() {
  memToken = '';
  memExp = 0;
  try {
    wx.removeStorageSync(TOKEN_KEY);
    wx.removeStorageSync(EXP_KEY);
  } catch (_) {
    // 忽略存储异常
  }
}

// 原始登录：wx.login 取一次性 code → POST /api/login 换令牌。
// 任何失败都返回 null（不抛错），由 getToken 转为空串的无鉴权模式。
function rawLogin() {
  return new Promise((resolve) => {
    wx.login({
      success(loginRes) {
        if (!loginRes || !loginRes.code) {
          resolve(null);
          return;
        }
        wx.request({
          url: config.baseURL + '/api/login',
          method: 'POST',
          data: { code: loginRes.code },
          header: { 'Content-Type': 'application/json' },
          timeout: 15000,
          success(res) {
            if (res.statusCode === 200 && res.data && res.data.token) {
              saveCache(res.data.token, res.data.expires_in);
              resolve(memToken);
            } else {
              resolve(null);
            }
          },
          fail() {
            resolve(null);
          },
        });
      },
      fail() {
        resolve(null);
      },
    });
  });
}

// 取可用令牌：有效返回令牌；无可用令牌时静默登录一次；
// 登录不可用时返回空串（调用方不带 Authorization 头）。
function getToken() {
  loadCache();
  if (memToken && memExp > Date.now() + GRACE_MS) {
    return Promise.resolve(memToken);
  }
  if (Date.now() < noAuthUntil) {
    return Promise.resolve('');
  }
  if (!loginPromise) {
    loginPromise = rawLogin().then((tok) => {
      loginPromise = null;
      if (!tok) {
        noAuthUntil = Date.now() + NOAUTH_RETRY_MS;
      }
      return tok || '';
    });
  }
  return loginPromise;
}

// 令牌失效（收到 401 后调用）：清缓存并允许立即重新登录。
function invalidate() {
  clearCache();
  noAuthUntil = 0;
}

module.exports = {
  getToken,
  invalidate,
};
