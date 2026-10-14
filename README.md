# 万能提取 · wanext

多平台视频 / 图片无水印解析下载工具（Go 实现，单二进制、零依赖）。

参考了 [opendry](https://github.com/packagelock/opendry) 的解析思路（平台自动识别、抖音 `playwm`→`play`、详情接口 + 兜底路径）
与 [video-parser](https://github.com/qinuan01/video-parser) 的产品形态（`POST /api/parse` + `/api/media/{token}` 同源代理、Range 预览、15 分钟令牌有效期），
用 Go 重写为单个可执行文件，并内置 Web 测试页面。

## 支持的平台

| 平台 | 链接形式 | 可获取 |
|---|---|---|
| 抖音 | `v.douyin.com` / `douyin.com` 短链、完整分享文案 | 无水印视频（图集→原图）、分享者身份（归因） |
| 快手 | `v.kuaishou.com` / `kuaishou.com` | 无水印视频 |
| 小红书 | `xhslink.com` / `xhslink.cn` / `xiaohongshu.com` | 视频 / 笔记原图 |
| 微博 | `weibo.com` 分享链接 | 视频（最高清晰度）/ 原图 |
| B站 | `bilibili.com` / `b23.tv`（支持 `?p=` 分 P） | 单段 MP4（含音轨） |

## 快速开始

```bash
# 编译（需 Go 1.21+）
go build -o wanext ./cmd/wanext

# 启动 Web 服务，用浏览器打开 http://127.0.0.1:8080 测试
./wanext serve
# 监听所有网卡（局域网/服务器部署）：
./wanext serve -addr 0.0.0.0:8080

# 命令行解析
./wanext parse "7.77 复制打开抖音 https://v.douyin.com/xxxx/ ..."
./wanext parse "https://www.bilibili.com/video/BV1xx411c7mD"

# 抖音分享者身份解析：还原「这条分享链接是谁分享的」（不是视频作者，是点转发的人）
./wanext sharer "7.77 复制打开抖音 https://v.douyin.com/xxxx/ ..."
```

## API

- `POST /api/parse` —— 请求体 `{"url":"<分享链接或文案>"}`，返回结构化结果：

```json
{
  "platform": "douyin",
  "title": "作品标题",
  "author": "作者",
  "cover_url": "封面",
  "page_url": "原帖地址",
  "medias": [
    {
      "kind": "video",
      "direct_url": "https://...（直链，可能带防盗链）",
      "proxy_url": "/api/media/<token>（同源代理，可直接播放）",
      "download_url": "/api/media/<token>?download=1（触发下载）"
    }
  ]
}
```

- `GET /api/media/{token}` —— 同源代理预览，支持 `Range` 断点续传；加 `?download=1` 作为附件下载。
- `GET /api/platforms` —— 支持的平台列表。
- `GET /api/health` —— 返回 `{"ok":true,"version":"0.1.0"}`，供小程序启动探测与部署探活，不鉴权。
- `POST /api/douyin/sharer` —— 抖音分享者身份解析（请求体同上），返回分享者与视频作者的公开资料：

```json
{
  "video_id": "作品 ID",
  "sharer": {"nickname": "分享者昵称", "douyin_id": "抖音号", "signature": "签名",
             "profile_url": "主页", "share_time": "2025-05-02 22:42:11"},
  "author": {"nickname": "视频作者昵称", "douyin_id": "抖音号", "signature": "签名", "profile_url": "主页"}
}
```

媒体令牌默认 15 分钟有效，过期后重新解析即可。配置 `BASE_URL`（或 `serve -base-url`）后，`proxy_url` / `download_url` 返回绝对地址（小程序用）；未配置时保持相对路径。解析接口按 IP 限流（默认每分钟 20 次、每天 300 次），超限返回 429。

## 工作原理（各平台）

- **抖音**：短链跳转 → 提取作品 ID → 访客 `ttwid` Cookie 调 `iesdouyin` 详情接口 → CDN 地址 `playwm`→`play`
  （水印由 App 客户端合成，CDN 文件本身无水印）；接口被签名拦截时兜底走 `video_id` 免签播放接口。
  附加功能——**分享者身份解析**（思路参考 [douyin-share-resolver](https://github.com/hiFOFA/douyin-share-resolver)）：
  只跟第一跳 302 取归因参数 → `activity_info` 与 `u_code`（23 进制）双通道提取分享者 uid 并交叉验证 →
  注册 `ttwid` → 数字 uid 反查公开资料。解析结果的 `sharer` 字段 / `POST /api/douyin/sharer` /
  `wanext sharer` 均可用；老版分享或私信转发不带归因参数时无法识别。
- **快手**：视频页 HTML 中提取 `photoUrl`。
- **小红书**：笔记页 `window.__INITIAL_STATE__` → `sns-webpic.xhscdn.com` 原图 / 视频流 `masterUrl`。
  `xsec_token` 一次性、短时有效，请用刚分享的新链接。
- **微博**：`mid62`→`mid10` → `ajax/statuses/show` 接口 → 按清晰度优先级选流 / `large` 原图。
- **B站**：`view` 接口取 CID → `playurl` 接口 `fnval=0` 取单段 MP4（含音轨，无需合并；未登录最高 720P 左右）。

## 微信小程序（wx-mp 分支）

- 小程序代码在 `miniprogram/`（原生，见 `miniprogram/README.md`）。
- 后端新增：`BASE_URL`（环境变量优先于 `-base-url`，未配置时保持相对路径）、`GET /api/health`、解析接口 IP 限流（每分钟 20 次、每天 300 次）。
- 本分支已移除 X / Instagram 支持；小程序端不展示平台名，交互围绕「万能提取」展开。

## 注意事项

- 平台接口经常变化，若某平台解析失败请把报错信息反馈给我，我来修。
- 请遵守各平台服务条款与创作者版权，仅用于个人学习研究，勿用于商业盗播。
- 如需走代理访问上游，设置标准环境变量 `HTTP_PROXY` / `HTTPS_PROXY` 即可。

## 免责声明

本工具仅供个人学习研究使用。请尊重各平台的服务条款和内容创作者的版权，
不要用于商业性转载或盗版传播。作者不对滥用造成的后果负责。
