# CLAUDE.md

给在本仓库工作的 AI / 开发者的项目约定与注意事项。

## 项目是什么

`zsh-agent`：一个最小、分层、便于学习的命令行 AI agent（Go 1.24，纯标准库）。
通过 **OpenAI 兼容接口**（`/v1/chat/completions`）调用大模型，让模型调用 `bash` 工具操作本地 zsh。详见 `README.md`。

## ⚠️ 安全（最重要）

- **本仓库是 GitHub 公开仓库**：提交/推送的任何内容都会公开可见、被搜索引擎索引，且 **git 历史不可逆**（即使后续删除也可能被缓存）。
- **严禁提交**：API key、真实端点 IP / 域名、内网地址、邮箱等敏感信息。
- 真实配置只放 `config.json`（已被 `.gitignore`，不提交）。文档 / 示例 / 测试里一律用**占位符**或 **RFC 5737 文档 IP**（如 `203.0.113.10`），不要写真实地址。
- 提交前务必扫描，例如：`grep -rnE "sk-[A-Za-z0-9]{8,}" . --include='*.go' --include='*.md' --include='*.json'`。

## 常用命令

```bash
make build         # = go build ./...
make vet           # = go vet ./...
make test          # 跑全部测试（含端到端集成测试）
make fmt-check     # 检查格式（提交前应无输出）
make lint          # golangci-lint（首次会按需安装固定版本到 $GOPATH/bin）
make check         # 提交前一把梭：build + vet + fmt-check + test + lint
go run .           # 运行（需先准备 config.json，见 README）
```

> golangci-lint 只是**开发 / CI 工具**，不被任何源码 import，不违反「零第三方依赖」。
> 版本固定在 `Makefile` 的 `GOLANGCI_LINT_VERSION`，升级改这一处即可。

## 架构与约定

- **分层 + 接口隔离**，核心循环（`agent/agent.go`）永不随扩展而改：
  - `llm.Provider`（`llm/`）—— LLM 后端抽象，OpenAI 协议细节封装在 `llm/openai.go`，不外泄。
  - `tools.Tool` + `tools.Registry`（`tools/`）—— 工具抽象。
  - `agent.UI`（`agent/`）—— 交互抽象，终端实现在 `agent/terminal.go`。
- 中立类型在 `llm/types.go`，是各层之间唯一的公共契约。
- 代码风格：**纯 Go 标准库、零第三方依赖**；**重中文注释**；一个文件一个概念。
- **加工具** = 实现 `tools.Tool` + 在 `main.go` 里 `reg.Register(...)` 一行。
- **换后端** = 实现 `llm.Provider`，在 `main.go` 换构造函数。
- 改动后请保证 `make build`、`make vet`、`make fmt-check`、`make test` 全部干净。
- **测试通过后必须再跑 `make lint`（golangci-lint）并清零告警**，是收尾 / 提交前的最后一关；图省事可直接 `make check` 一把梭。

## 设计文档

`docs/superpowers/specs/` 下有完整的设计 spec，迭代前可先读。
