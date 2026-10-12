// Package model 定义解析结果的数据结构。
package model

// Media 表示一个可下载的媒体（视频或图片）。
type Media struct {
	Kind      string `json:"kind"` // "video" | "image"
	DirectURL string `json:"direct_url"`
	CoverURL  string `json:"cover_url,omitempty"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
}

// Result 表示一次解析的结果。
type Result struct {
	Platform string  `json:"platform"`
	Title    string  `json:"title"`
	Author   string  `json:"author"`
	CoverURL string  `json:"cover_url,omitempty"`
	PageURL  string  `json:"page_url,omitempty"`
	Medias   []Media `json:"medias"`
}
