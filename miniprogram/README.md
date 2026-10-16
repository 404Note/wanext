# 任意取 · 微信小程序（wx-mp 分支）

原生小程序（WXML / WXSS / JS），一页闭环：输入内容 → 提取 → 轮播预览 → 保存当前 / 复制。复用 wanext Go 后端，不改动任何后端提取逻辑。

## 配置

1. 复制 `miniprogram/config.local.example.js` 为 `miniprogram/config.local.js` 后填入真实域名（如 `https://api.example.com`）。不要改 `config.js`（占位回退）。本地开发可用 `http://127.0.0.1:8080`，开发者工具需勾选“不校验合法域名”。
2. `miniprogram/project.config.json` 的 `appid` 改成真实 AppID（当前为 `touristappid` 占位，仅供本地开发）。
3. 小程序后台：同一备案域名同时配进 request 合法域名与 downloadFile 合法域名两栏；隐私指引按实际调用填写（粘贴板：用户主动点击才读；相册写入：保存时授权）。
4. 克隆后运行仓库根目录的 `scripts/setup-hooks.sh` 启用防提交钩子；真实配置与密钥绝不提交（见仓库 README 的配置与密钥一节）。

## 后端

```bash
# 本地
./wanext serve -addr 127.0.0.1:8080

# 公网（反向代理终止 TLS，转发到回环地址）
BASE_URL=https://api.example.com ./wanext serve -addr 127.0.0.1:8080
```

或复制根目录 `.env.example` 为 `.env` 后填入，启动时自动加载（进程环境变量优先于 `.env`）。

- `BASE_URL` 未配置时，`proxy_url` / `download_url` 保持相对路径，Web 页不受影响。
- `GET /api/health` 返回 `{"ok":true,"version":"0.1.0"}`，供启动探测与部署探活。
- 提取接口按 IP 限流（默认每分钟 20 次、每天 300 次），超限返回 429。登录校验未启用：上线前如需开启，另行接入。

## 前端约定

- 文案只围绕「任意取」展开，不出现任何来源名称，不直接描述功能。
- 仅取后端返回的标题、作者与媒体列表，其余字段不使用；媒体按返回顺序展示，不重排、不按类型分组。
- 用户侧失败文案统一为中性文案（如“提取失败，请重试”），不透传服务端细节；媒体地址过期时自动用原输入重新提取一次。
- 轮播用 `swiper` 按原始顺序混排，切换页时暂停上一页；保存用临时文件 + 存入相册，拒绝授权时引导去设置页开启，开启后自动续走一次。

## 本地验证

微信开发者工具导入 `miniprogram/` 目录，真机预览走体验版；后端回归用 `go test ./...`（改动包需 `go vet` 通过）。
