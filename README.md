# zsh-agent

一个**最小但结构清晰**的命令行 AI agent，用来学习"agent 到底是怎么转起来的"。

它通过 **OpenAI 兼容接口**（`/v1/chat/completions`）调用一个大模型当大脑，让模型调用 `bash` 工具来操作你本地的 zsh，完成你用自然语言提出的需求。

- 纯 Go 标准库，**零第三方依赖**
- 全程重中文注释，照着读就能搞懂 agent 的工作原理
- 分层设计：核心循环一眼读懂，"会变的东西"都能轻松替换

---

## 快速开始

**1. 准备配置**

复制示例并填上你的真实信息：

```bash
cp config.example.json config.json
```

编辑 `config.json`：

```json
{
  "base_url": "https://203.0.113.10:443/v1",
  "api_key": "sk-你的真实key",
  "model": "qwen3:8b",
  "max_tokens": 4096
}
```

> `config.json` 含密钥，已在 `.gitignore` 里，不会进版本库。

**2. 运行**

```bash
go run .
# 或
go build -o zsh-agent . && ./zsh-agent
```

**3. 用它**

```
zsh-agent 已就绪（后端 https://203.0.113.10:443/v1，模型 qwen3:8b）。输入需求后回车，Ctrl+D 退出。

> 看看当前目录有多少个 go 文件

▶ bash {"command":"ls *.go | wc -l"}
执行？[Y/n] y
3
```

模型每次想执行命令都会先让你 `Y/n` 确认，回车默认同意，输入 `n` 拒绝。

---

## 配置说明

| 字段 | 必填 | 说明 |
|---|---|---|
| `base_url` | 是 | OpenAI 兼容接口根地址，例如 `https://203.0.113.10:443/v1`。程序会在其后拼 `/chat/completions` |
| `api_key` | 是 | 访问后端的 Bearer token |
| `model` | 是 | 模型名，例如 `qwen3:8b` |
| `max_tokens` | 否 | 单次回复 token 上限，默认 `4096` |
| `system_prompt` | 否 | 自定义系统提示词，留空用内置默认 |
| `stream` | 否 | 是否用 SSE 流式接收，默认 `false`。开启后思考与正文逐字实时显示（需后端支持 `stream:true`） |
| `insecure_skip_verify` | 否 | 手动控制是否跳过 TLS 证书校验。不写则按地址自动判断（见下） |

配置文件路径默认 `./config.json`，可用环境变量覆盖：

```bash
AGENT_CONFIG=/path/to/other.json go run .
```

### 关于 HTTPS 与纯 IP（为什么 curl 要加 `-k`）

直连 **纯 IP 端点**（如 `203.0.113.10:443`）时，服务端证书通常没法被正常校验（自签 / 不匹配该 IP），等价于 `curl` 要加 `-k`。本程序会自动识别：

- `base_url` 主机名是**纯 IP** → 自动跳过证书校验（相当于 `-k`）
- 主机名是**域名** → 走正常证书校验

判断逻辑见 `llm/openai.go` 的 `shouldSkipVerify`。需要时也可用配置里的 `insecure_skip_verify` 手动覆盖（例如域名 + 自签证书的场景）。

---

## 架构：一个 agent 是怎么转的

agent 的本质就一句话——**循环地「问模型 → 模型要调工具就执行 → 把结果喂回去」，直到模型不再要工具**。

这就是 `agent/agent.go` 里 `Run` 做的事：

```
用户输入 ─▶ history(+user) ─▶ 问模型 ─▶ 模型要调工具？
                ▲                          │否            │是
                │                          ▼              ▼
                │                   收工，返回历史   逐个执行工具（先经 UI 确认）
                └────────────────── 把工具结果追加进 history ◀┘
```

框架把"会变的三类东西"各用一个接口隔离，于是**核心循环永远不用改**：

| 会变的东西 | 隔离接口 | 怎么扩展 |
|---|---|---|
| LLM 后端 | `llm.Provider` | 新写一个文件实现接口 |
| 工具 | `tools.Tool` + `Registry` | 写个工具文件 + 注册一行 |
| 交互方式 | `agent.UI` | 换个 UI 实现 |

---

## 目录结构

```
main.go                 组装：读配置 → 建 provider → 注册工具 → 跑 agent
config/
  config.go             JSON 配置：加载 + 校验 + 默认值
llm/
  types.go              provider 中立的类型（Message / ToolCall / ToolSpec / Response）
  provider.go           Provider 接口
  openai.go             OpenAI 兼容实现：协议翻译 + 纯 IP 免校验 TLS
tools/
  tool.go               Tool 接口 + Registry
  bash.go               bash 工具（zsh -c 执行）
agent/
  ui.go                 UI 接口
  terminal.go           终端 UI（展示 / y-n 确认）
  agent.go              核心循环 —— 全项目的心脏
docs/superpowers/specs/  设计文档
```

建议的阅读顺序（学习路线）：
`llm/types.go` → `agent/agent.go`（心脏）→ `llm/provider.go` → `tools/tool.go` → `llm/openai.go` → `main.go`。

---

## 怎么扩展

**加一个工具**：新建 `tools/readfile.go` 实现 `Tool` 接口，然后在 `main.go` 里加一行：

```go
reg.Register(tools.ReadFile{})
```

核心循环、其它工具都不用动。

**换一个 LLM 后端**：新建 `llm/anthropic.go` 实现 `Provider` 接口，在 `main.go` 里换掉构造函数即可。循环、工具都不用动。

**换交互方式**（比如做成 HTTP 服务）：实现 `agent.UI` 接口，复用 `Agent.Run`。

---

## 测试

```bash
go test ./...
```

覆盖：TLS 判断（纯 IP / 域名 / 手动覆盖）、OpenAI 协议双向翻译、配置校验与默认值、bash 执行，以及一个用假 OpenAI 服务跑完整链路的端到端测试（`agent/integration_test.go`）。
