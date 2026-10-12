// Package model 定义解析结果的数据结构。
package model

// Media 表示一个可下载的媒体（视频或图片）。
type Media struct {
	Kind      string `json:"kind"` // "video" | "image"
	DirectURL string `json:"direct_url"`
	CoverURL  string `json:"cover_url,omitempty"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	// Cookie 代理下载该媒体时需附带的 Cookie（如 TikTok playAddr 要求
	// tt_chain_token Cookie 与 URL 中 tk 参数绑定）。不序列化到前端。
	Cookie string `json:"-"`
}

// SharerInfo 抖音分享链接的分享者公开身份（由分享链接归因参数解析）。
type SharerInfo struct {
	Nickname   string `json:"nickname"`
	DouyinID   string `json:"douyin_id,omitempty"`   // 抖音号
	Signature  string `json:"signature,omitempty"`   // 个性签名
	ProfileURL string `json:"profile_url,omitempty"` // 主页地址
	ShareTime  string `json:"share_time,omitempty"`  // 分享时刻（北京时间），仅分享者有
}

// Result 表示一次解析的结果。
type Result struct {
	Platform string      `json:"platform"`
	Title    string      `json:"title"`
	Author   string      `json:"author"`
	CoverURL string      `json:"cover_url,omitempty"`
	PageURL  string      `json:"page_url,omitempty"`
	Medias   []Media     `json:"medias"`
	Sharer   *SharerInfo `json:"sharer,omitempty"` // 分享者身份（仅抖音短链分享，且归因参数可用时）
}
