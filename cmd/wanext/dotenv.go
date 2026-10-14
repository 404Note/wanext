package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// loadDotEnv 加载同目录下的 .env 文件：仅补齐进程环境变量中未设置的键，
// 进程环境变量优先。文件不存在时静默跳过；格式错误的行只警告，不中断启动。
// 真实密钥只放 .env（未追踪，绝不提交），.env.example 仅为占位模板。
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		k, v, ok := parseDotEnvLine(sc.Text())
		if !ok {
			continue
		}
		if _, exists := os.LookupEnv(k); !exists {
			_ = os.Setenv(k, v)
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "警告: 读取 .env 失败:", err)
	}
}

// parseDotEnvLine 解析单行 KEY=VALUE：支持 export 前缀与单双引号，
// 忽略空行、注释与格式错误的行（警告中不回显行内容，避免泄露密钥）。
func parseDotEnvLine(line string) (string, string, bool) {
	s := strings.TrimSpace(line)
	if s == "" || strings.HasPrefix(s, "#") {
		return "", "", false
	}
	if strings.HasPrefix(s, "export ") {
		s = strings.TrimSpace(strings.TrimPrefix(s, "export "))
	}
	i := strings.Index(s, "=")
	if i <= 0 {
		fmt.Fprintln(os.Stderr, "警告: .env 有格式错误的行，已忽略")
		return "", "", false
	}
	k := strings.TrimSpace(s[:i])
	v := strings.TrimSpace(s[i+1:])
	if v == "" || (v[0] != '"' && v[0] != '\'') {
		if j := strings.Index(v, " #"); j >= 0 {
			v = strings.TrimSpace(v[:j])
		}
	}
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
	}
	if k == "" {
		return "", "", false
	}
	return k, v, true
}
