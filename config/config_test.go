package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTempConfig 把一段 JSON 文本写进 t.TempDir() 下的临时文件，返回其路径。
// 临时目录会在测试结束后由 testing 自动清理，互不干扰。
func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写入临时配置失败: %v", err)
	}
	return path
}

// TestLoadValid 验证：一份合法配置能被正确解析，且默认值被填好。
func TestLoadValid(t *testing.T) {
	path := writeTempConfig(t, `{
		"base_url": "https://203.0.113.10:443/v1",
		"api_key": "sk-test",
		"model": "qwen3:8b",
		"max_tokens": 2048,
		"system_prompt": "你是个助手"
	}`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("期望解析成功，却报错: %v", err)
	}
	if cfg.BaseURL != "https://203.0.113.10:443/v1" {
		t.Errorf("BaseURL 解析错误: %q", cfg.BaseURL)
	}
	if cfg.APIKey != "sk-test" {
		t.Errorf("APIKey 解析错误: %q", cfg.APIKey)
	}
	if cfg.Model != "qwen3:8b" {
		t.Errorf("Model 解析错误: %q", cfg.Model)
	}
	if cfg.MaxTokens != 2048 {
		t.Errorf("MaxTokens 应为显式值 2048，却得到 %d", cfg.MaxTokens)
	}
	if cfg.SystemPrompt != "你是个助手" {
		t.Errorf("SystemPrompt 解析错误: %q", cfg.SystemPrompt)
	}
}

// TestLoadMissingFile 验证：文件不存在时，错误信息里要带上路径，便于定位。
func TestLoadMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "不存在.json")
	_, err := Load(path)
	if err == nil {
		t.Fatal("文件不存在时应当返回错误，却返回了 nil")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("错误信息应包含路径 %q，实际为: %v", path, err)
	}
}

// TestLoadInvalidJSON 验证：JSON 格式错误时返回错误（且不 panic）。
func TestLoadInvalidJSON(t *testing.T) {
	path := writeTempConfig(t, `{ this is not json `)
	if _, err := Load(path); err == nil {
		t.Fatal("非法 JSON 应当返回错误，却返回了 nil")
	}
}

// TestLoadMissingRequired 用子测试逐个检查：缺任一必填字段都会报错，
// 且错误信息里点名了缺失的那个字段。
func TestLoadMissingRequired(t *testing.T) {
	cases := []struct {
		name      string // 子测试名
		content   string // 故意缺一个字段的配置
		wantField string // 错误信息里应出现的字段名
	}{
		{
			name:      "缺 base_url",
			content:   `{"api_key": "sk-test", "model": "qwen3:8b"}`,
			wantField: "base_url",
		},
		{
			name:      "缺 api_key",
			content:   `{"base_url": "https://x/v1", "model": "qwen3:8b"}`,
			wantField: "api_key",
		},
		{
			name:      "缺 model",
			content:   `{"base_url": "https://x/v1", "api_key": "sk-test"}`,
			wantField: "model",
		},
		{
			name:      "字段存在但为空字符串也算缺",
			content:   `{"base_url": "", "api_key": "sk-test", "model": "qwen3:8b"}`,
			wantField: "base_url",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempConfig(t, tc.content)
			_, err := Load(path)
			if err == nil {
				t.Fatalf("缺少 %s 时应返回错误，却返回了 nil", tc.wantField)
			}
			if !strings.Contains(err.Error(), tc.wantField) {
				t.Errorf("错误信息应点名字段 %q，实际为: %v", tc.wantField, err)
			}
		})
	}
}

// TestMaxTokensDefault 验证：未配置 max_tokens 时，Load 会填默认值 4096。
func TestMaxTokensDefault(t *testing.T) {
	path := writeTempConfig(t, `{
		"base_url": "https://x/v1",
		"api_key": "sk-test",
		"model": "qwen3:8b"
	}`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if cfg.MaxTokens != 4096 {
		t.Errorf("未配置 max_tokens 时应默认 4096，实际为 %d", cfg.MaxTokens)
	}
}

// TestInsecureSkipVerify 验证 *bool 的三态语义：
//   - 配置里写了 → 指针非 nil，且值正确（区分 true / false）。
//   - 配置里没写 → 指针保持 nil（交给上层「按 IP 自动判断」）。
func TestInsecureSkipVerify(t *testing.T) {
	t.Run("显式 true", func(t *testing.T) {
		path := writeTempConfig(t, `{
			"base_url": "https://x/v1", "api_key": "k", "model": "m",
			"insecure_skip_verify": true
		}`)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if cfg.InsecureSkipVerify == nil {
			t.Fatal("配置里写了 insecure_skip_verify，指针不应为 nil")
		}
		if *cfg.InsecureSkipVerify != true {
			t.Errorf("期望 true，实际 %v", *cfg.InsecureSkipVerify)
		}
	})

	t.Run("显式 false", func(t *testing.T) {
		path := writeTempConfig(t, `{
			"base_url": "https://x/v1", "api_key": "k", "model": "m",
			"insecure_skip_verify": false
		}`)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if cfg.InsecureSkipVerify == nil {
			t.Fatal("配置里写了 false，指针仍不应为 nil（false 与未配置要区分）")
		}
		if *cfg.InsecureSkipVerify != false {
			t.Errorf("期望 false，实际 %v", *cfg.InsecureSkipVerify)
		}
	})

	t.Run("未配置保持 nil", func(t *testing.T) {
		path := writeTempConfig(t, `{
			"base_url": "https://x/v1", "api_key": "k", "model": "m"
		}`)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if cfg.InsecureSkipVerify != nil {
			t.Errorf("未配置时指针应为 nil，实际指向 %v", *cfg.InsecureSkipVerify)
		}
	})
}

// TestUnknownFieldsIgnored 验证向前兼容：配置里出现未知字段时不应报错。
func TestUnknownFieldsIgnored(t *testing.T) {
	path := writeTempConfig(t, `{
		"base_url": "https://x/v1", "api_key": "k", "model": "m",
		"future_field": "whatever", "another": 123
	}`)
	if _, err := Load(path); err != nil {
		t.Errorf("未知字段应被忽略（向前兼容），却报错: %v", err)
	}
}
