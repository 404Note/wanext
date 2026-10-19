// wanext —— 多平台视频/图片无水印解析下载工具。
//
//	wanext serve [-addr 127.0.0.1:8080]   启动 Web 服务（含测试页面）
//	wanext parse "<分享文案或链接>"        命令行解析并输出 JSON
//	wanext sharer "<抖音分享文案或短链>"    解析分享者身份（这条链接是谁分享的）
//	wanext version                        显示版本
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"wanext/internal/extract"
	"wanext/internal/platform"
	"wanext/internal/server"
)

const version = "0.1.0"

func main() {
	loadDotEnv(".env")
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ExitOnError)
		addr := fs.String("addr", "127.0.0.1:8080", "监听地址")
		baseURL := fs.String("base-url", "", "对外绝对地址前缀，如 https://api.example.com（环境变量 BASE_URL 优先）")
		_ = fs.Parse(os.Args[2:])
		cfg, err := server.ConfigFromEnv()
		if err != nil {
			fmt.Fprintln(os.Stderr, "配置错误:", err)
			os.Exit(1)
		}
		if cfg.BaseURL == "" {
			cfg.BaseURL = *baseURL
		}
		fmt.Printf("wanext v%s Web 服务已启动：http://%s\n", version, *addr)
		fmt.Println("在浏览器打开上面的地址即可测试解析（Ctrl+C 退出）。")
		if err := server.ServeWithConfig(*addr, cfg); err != nil {
			fmt.Fprintln(os.Stderr, "启动失败:", err)
			os.Exit(1)
		}
	case "parse":
		text := strings.Join(os.Args[2:], " ")
		if strings.TrimSpace(text) == "" {
			fmt.Fprintln(os.Stderr, "用法: wanext parse \"<分享文案或链接>\"")
			os.Exit(2)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		res, err := extract.ParseText(ctx, text)
		if err != nil {
			fmt.Fprintln(os.Stderr, "解析失败:", err)
			os.Exit(1)
		}
		b, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(b))
	case "sharer":
		// 抖音分享者身份解析：还原「这条分享链接是谁分享的」
		text := strings.Join(os.Args[2:], " ")
		if strings.TrimSpace(text) == "" {
			fmt.Fprintln(os.Stderr, "用法: wanext sharer \"<抖音分享文案或短链>\"")
			os.Exit(2)
		}
		shortURL := ""
		for _, u := range extract.ExtractURLs(text) {
			if strings.Contains(u, "v.douyin.com") {
				shortURL = u
				break
			}
		}
		if shortURL == "" {
			fmt.Fprintln(os.Stderr, "未从输入中找到 v.douyin.com 短链")
			os.Exit(1)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		res, err := platform.Douyin{}.ResolveSharer(ctx, shortURL)
		if err != nil {
			fmt.Fprintln(os.Stderr, "解析失败:", err)
			os.Exit(1)
		}
		b, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(b))
	case "version":
		fmt.Println("wanext", version)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Println(`wanext v` + version + ` —— 多平台视频/图片无水印解析下载

用法:
  wanext serve [-addr 127.0.0.1:8080] [-base-url https://api.example.com]   启动 Web 服务，在浏览器中测试
  wanext parse "<分享文案或链接>"         命令行解析，输出 JSON
  wanext sharer "<抖音分享文案或短链>"    解析分享者身份（这条链接是谁分享的）
  wanext version                        显示版本

环境变量:
  BASE_URL                              对外绝对地址前缀（小程序用，优先于 -base-url）
  AUTH_REQUIRED                         1=强制微信登录鉴权（缺密钥启动失败），0=关闭（缺省，本地开发）
  WX_APPID / WX_SECRET                  微信小程序 AppID 与 AppSecret（鉴权开启时必填，只进 .env）
  JWT_SECRET                            JWT 签名密钥（≥32 字节，鉴权开启时必填，只进 .env）
  BANNED_UIDS                           紧急兜底封禁名单（uid 逗号分隔，管理页不可用时用）
  ADMIN_ENABLED                         1=启用管理页（需同时配置 ADMIN_USER / ADMIN_PASSWORD）
  ADMIN_USER / ADMIN_PASSWORD           管理员账号密码（密码≥6 位，只进 .env）
  ADMIN_DATA_DIR                        管理页状态文件目录（缺省 data）
  BASE_PATH                             服务挂载的子路径前缀，如 /wx-mp/wanext（nginx 反代子路径部署用，缺省根路径）
  HTTP_PROXY / HTTPS_PROXY              上游请求代理（如需）`)
}
