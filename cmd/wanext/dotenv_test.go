package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseDotEnvLine(t *testing.T) {
	cases := []struct {
		line     string
		wantK    string
		wantV    string
		wantOK   bool
	}{
		{"", "", "", false},
		{"# 注释", "", "", false},
		{"BASE_URL=https://api.example.com", "BASE_URL", "https://api.example.com", true},
		{"export BASE_URL=https://api.example.com", "BASE_URL", "https://api.example.com", true},
		{`SAMPLE_KEY="abc123"`, "SAMPLE_KEY", "abc123", true},
		{`SAMPLE_KEY='abc123'`, "SAMPLE_KEY", "abc123", true},
		{"BASE_URL=https://api.example.com # 行尾注释", "BASE_URL", "https://api.example.com", true},
		{"没有等号", "", "", false},
		{"=没有键", "", "", false},
		{"EMPTY=", "EMPTY", "", true},
	}
	for _, c := range cases {
		k, v, ok := parseDotEnvLine(c.line)
		if k != c.wantK || v != c.wantV || ok != c.wantOK {
			t.Errorf("parseDotEnvLine(%q) = (%q, %q, %v)，期望 (%q, %q, %v)",
				c.line, k, v, ok, c.wantK, c.wantV, c.wantOK)
		}
	}
}

func TestLoadDotEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := "TEST_DOTENV_A=from-file\nTEST_DOTENV_B=from-file\n# 注释\n格式错误行\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_DOTENV_B", "from-process")
	loadDotEnv(path)
	if got := os.Getenv("TEST_DOTENV_A"); got != "from-file" {
		t.Errorf(".env 未生效：TEST_DOTENV_A = %q", got)
	}
	if got := os.Getenv("TEST_DOTENV_B"); got != "from-process" {
		t.Errorf("进程环境变量应优先于 .env：TEST_DOTENV_B = %q", got)
	}
	// 不存在的文件应静默跳过，不报错
	loadDotEnv(filepath.Join(dir, "不存在.env"))
}
