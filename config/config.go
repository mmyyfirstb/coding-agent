// Package config 负责「从磁盘读取一份 JSON 配置 → 校验 → 填默认值 → 交给上层用」。
//
// 设计取向：
//   - 只依赖 Go 标准库（os / encoding/json / fmt），零第三方依赖。
//   - 配置里既有「连哪个大模型后端」的基本信息（base_url / api_key / model），
//     也有少量可选项（max_tokens / system_prompt / insecure_skip_verify）。
//   - 校验与默认值集中在 Load 一处完成，调用方拿到的 *Config 一定是「已就绪」的。
package config

import (
	"encoding/json"
	"fmt"
	"os"
)

// Config 是整个 agent 的运行配置，字段与 config.json 一一对应。
//
// 注意 json tag 的写法：
//   - 必填项（base_url / api_key / model）不加 omitempty，语义上要求出现。
//   - 可选项加 omitempty，序列化时若为零值会被省略，配置文件更干净。
type Config struct {
	// BaseURL 是 OpenAI 兼容接口的根地址，例如 https://203.0.113.10:443/v1。
	// 必填。后续 Provider 会在它后面拼 /chat/completions 之类的路径。
	BaseURL string `json:"base_url"` // 如 https://203.0.113.10:443/v1

	// APIKey 是访问后端用的密钥（Bearer token），会放进 Authorization 头。
	// 必填。属于敏感信息，所以真实的 config.json 不进版本库（见 .gitignore）。
	APIKey string `json:"api_key"` // Bearer token

	// Model 指定要调用的模型名，例如 qwen3:8b。必填。
	Model string `json:"model"` // 如 qwen3:8b

	// MaxTokens 限制单次回复的最大 token 数。可选；为 0（即未配置）时 Load 会填 4096。
	MaxTokens int `json:"max_tokens,omitempty"` // 默认 4096

	// SystemPrompt 是可选的系统提示词，用来给模型设定人设 / 规则。
	// 留空则由上层决定是否使用内置默认提示。
	SystemPrompt string `json:"system_prompt,omitempty"`

	// InsecureSkipVerify 控制 HTTPS 是否跳过证书校验。
	//
	// 这里用 *bool（指针）而不是 bool，是为了区分「三态」：
	//   - nil   ：配置里没写 → 由上层「按 IP 自动判断」（比如直连 IP 的自签证书场景）。
	//   - 非 nil ：配置里明确写了 true/false → 手动覆盖自动判断。
	// 普通 bool 无法区分「没写」和「写了 false」，所以才用指针。
	InsecureSkipVerify *bool `json:"insecure_skip_verify,omitempty"` // nil=按 IP 自动判断；非 nil=手动覆盖
}

// Load 从 path 读取并解析配置，完成校验与默认值填充后返回。
//
// 流程：
//  1. 读文件——文件不存在 / 读不了时给出带路径的清晰中文错误。
//  2. JSON 反序列化——故意不开 DisallowUnknownFields，
//     这样以后配置新增字段时旧代码也能读（向前兼容）。
//  3. 校验三个必填字段非空，缺哪个就在错误里点名哪个。
//  4. MaxTokens 为 0 时填默认值 4096。
func Load(path string) (*Config, error) {
	// 1. 读文件。把底层错误一并带上，方便定位（权限、路径拼错等）。
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败 %q: %w", path, err)
	}

	// 2. 反序列化。不使用 DisallowUnknownFields —— 未知字段直接忽略，保证向前兼容。
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件 %q 失败（JSON 格式有误）: %w", path, err)
	}

	// 3. 校验必填项。逐个点名，错误信息直接告诉用户该补哪个字段。
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("配置缺少必填字段 base_url（如 https://203.0.113.10:443/v1）")
	}
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("配置缺少必填字段 api_key（访问后端用的 Bearer token）")
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("配置缺少必填字段 model（如 qwen3:8b）")
	}

	// 4. 填默认值：没配 max_tokens（值为 0）时用 4096。
	if cfg.MaxTokens == 0 {
		cfg.MaxTokens = 4096
	}

	return &cfg, nil
}
