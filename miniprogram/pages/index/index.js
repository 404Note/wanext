const api = require('../../utils/api.js');
const clipboard = require('../../utils/clipboard.js');
const save = require('../../utils/save.js');

function mapError(err) {
  if (!err) return '提取失败，请重试';
  if (err.code === 'network') return '网络异常，请检查网络后重试';
  if (err.code === 'http') {
    if (err.status === 400) return '没有识别到内容，请检查后重试';
    if (err.status === 429) return '操作太频繁，请稍后再试';
    if (err.status === 502) return '提取失败，请重试';
  }
  return '提取失败，请重试';
}

Page({
  data: {
    inputText: '',
    status: 'idle',
    errorMsg: '',
    result: null,
    currentIndex: 0,
    saving: false,
    saveProgress: 0,
  },

  onInput(e) {
    this.setData({ inputText: e.detail.value });
  },

  onPaste() {
    clipboard
      .getClipboardText()
      .then((text) => {
        if (text) {
          this.setData({ inputText: text });
          return;
        }
        wx.showToast({ title: '剪贴板没有内容', icon: 'none' });
      })
      .catch(() => {
        wx.showToast({ title: '读取失败，请手动输入', icon: 'none' });
      });
  },

  onClear() {
    this.setData({
      inputText: '',
      status: 'idle',
      errorMsg: '',
      result: null,
      currentIndex: 0,
    });
    this._lastInput = '';
    this._retried = false;
  },

  onExtract() {
    const input = (this.data.inputText || '').trim();
    if (!input) {
      wx.showToast({ title: '请先输入内容', icon: 'none' });
      return;
    }
    this._doExtract(input, false);
  },

  onRetry() {
    const input = (this.data.inputText || '').trim();
    if (!input) return;
    this._doExtract(input, false);
  },

  // 核心提取：silent 为 true 时不切 loading 状态（用于过期自动恢复）。
  _doExtract(input, silent) {
    if (!silent) {
      this.setData({ status: 'loading', errorMsg: '' });
      // 用户手动发起新的提取，重置自动恢复标记；静默重提取不重置，
      // 否则 onMediaError 成功后会把标记清掉，导致失败时无限循环重提取。
      this._retried = false;
    }
    this._lastInput = input;
    return api
      .extractContent(input)
      .then((result) => {
        this.setData({
          status: 'success',
          errorMsg: '',
          result,
          currentIndex: 0,
        });
      })
      .catch((err) => {
        this.setData({ status: 'error', errorMsg: mapError(err) });
      });
  },

  onSwiperChange(e) {
    const prev = this.data.currentIndex;
    const current = e.detail.current;
    if (prev !== current) {
      try {
        const ctx = wx.createVideoContext('video-' + prev, this);
        if (ctx) ctx.pause();
      } catch (_) {
        // 忽略：当前项无对应上下文
      }
    }
    this.setData({ currentIndex: current });
  },

  // 预览失败且疑似过期时，用原输入静默重新提取一次。
  onMediaError() {
    if (this._retried || !this._lastInput) return;
    this._retried = true;
    this._doExtract(this._lastInput, true).then(() => {
      if (this.data.status === 'success') {
        wx.showToast({ title: '已为你重新提取', icon: 'none' });
      }
    });
  },

  onSaveCurrent() {
    const { result, currentIndex, saving } = this.data;
    if (!result || saving) return;
    const media = result.medias[currentIndex];
    if (!media || !media.proxyUrl) return;
    this._doSave(media, false);
  },

  _doSave(media, retried) {
    this.setData({ saving: true, saveProgress: 0 });
    return save
      .saveMedia(media.proxyUrl, media.kind, (progress) => {
        this.setData({ saveProgress: progress });
      })
      .then(() => {
        this.setData({ saving: false, saveProgress: 0 });
        wx.showToast({ title: '已保存到相册', icon: 'success' });
      })
      .catch((err) => {
        // 临时地址过期：用原输入重新提取一次，再重试保存一次。
        if (err && err.code === 'expired' && !retried && this._lastInput) {
          return this._doExtract(this._lastInput, true).then(() => {
            const { result, currentIndex } = this.data;
            const next = result && result.medias[currentIndex];
            this.setData({ saving: false, saveProgress: 0 });
            if (next && next.proxyUrl) {
              return this._doSave(next, true);
            }
            wx.showToast({ title: '内容已过期，请重新提取', icon: 'none' });
          });
        }
        this.setData({ saving: false, saveProgress: 0 });
        // 未授权相册：弹窗说明用途，引导去设置页开启，开启后自动续走一次。
        if (save.isAuthDeny(err)) {
          wx.showModal({
            title: '需要相册权限',
            content: '保存需要写入相册权限，请在设置中开启',
            confirmText: '去设置',
            cancelText: '取消',
            success: (res) => {
              if (!res.confirm) return;
              wx.openSetting({
                success: (setting) => {
                  if (setting.authSetting && setting.authSetting['scope.writePhotosAlbum']) {
                    this._doSave(media, true);
                  }
                },
              });
            },
          });
          return;
        }
        wx.showToast({ title: '保存失败，请重试', icon: 'none' });
      });
  },

  onCopy() {
    const { result, currentIndex } = this.data;
    const media = result && result.medias[currentIndex];
    if (!media || !media.directUrl) return;
    wx.setClipboardData({
      data: media.directUrl,
      success() {
        wx.showToast({ title: '已复制', icon: 'success' });
      },
    });
  },
});
