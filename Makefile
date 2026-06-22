# Makefile —— zsh-agent 常用开发命令。
# 纯标准库项目，目标尽量薄；配方一律 Tab 缩进。
# 注意：golangci-lint 装到 $(GOPATH)/bin，该目录未必在 PATH 上，
# 故一律用绝对路径 $(GOLANGCI_LINT) 调用，避免「装了却找不到」。

GO ?= go
GOLANGCI_LINT_VERSION ?= v2.12.2
GOPATH_BIN := $(shell $(GO) env GOPATH)/bin
GOLANGCI_LINT := $(GOPATH_BIN)/golangci-lint

.PHONY: build vet test fmt fmt-check lint golangci-lint-install check

# build：编译全部包。
build:
	$(GO) build ./...

# vet：标准库静态检查。
vet:
	$(GO) vet ./...

# test：跑全部测试（含端到端集成测试）。
test:
	$(GO) test ./...

# fmt：就地格式化全部代码。
fmt:
	gofmt -w .

# fmt-check：检查格式，有未格式化文件即失败（提交前应无输出）。
fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "需要 gofmt 的文件："; echo "$$out"; exit 1; fi

# golangci-lint-install：缺失或版本不符时，安装固定版本的 golangci-lint。
golangci-lint-install:
	@if ! $(GOLANGCI_LINT) version 2>/dev/null | grep -q "$(GOLANGCI_LINT_VERSION:v%=%)"; then \
		echo "Installing golangci-lint $(GOLANGCI_LINT_VERSION) ..."; \
		$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION); \
	fi

# lint：按需安装后跑 golangci-lint。
lint: golangci-lint-install
	$(GOLANGCI_LINT) run --timeout 5m ./...

# check：提交前一把梭——构建、静态检查、格式检查、测试、lint。
check: build vet fmt-check test lint
