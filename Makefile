.PHONY: build build-linux test run tidy vet lint docs verify qwen-asr-evals

BIN := bin/gateway

# golangci-lint v2 版本钉死,与 ci.yml 一致,本地/CI 同一门禁。
GOLANGCI_VERSION := v2.6.1

build:
	go build -trimpath -o $(BIN) ./cmd/gateway

# Linux/amd64 静态二进制(部署目标),无 CGO。
build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o $(BIN)-linux-amd64 ./cmd/gateway

test:
	go test -race ./...

run:
	go run ./cmd/gateway

tidy:
	go mod tidy

vet:
	go vet ./...

# CI-1 质量+安全门 + 架构分层门(depguard);本地复跑 ci.yml 的 lint job。
lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION) run ./...

# 文档治理门禁(GOVERNANCE §11):frontmatter/类型·状态/INDEX≤50/孤儿链接/working 90 天。
# cmd/docs 在文档治理切片落地后接入;未落地前 no-op 提示。
docs:
	@if [ -d cmd/docs ]; then go run ./cmd/docs; else echo "make docs: cmd/docs 未落地(文档治理门禁切片);暂跳过"; fi

# 本地门禁:vet + build + race 测试全绿(cmd/docs 落地后并入 docs)。
verify: vet build test docs

# 显式付费 live eval：真实连接 DashScope realtime ASR，经本地 gateway speech proxy 路径完成一次会话。
# 必须由调用者提供 DASHSCOPE_API_KEY/EVALS_KEY 与 DASHSCOPE_WORKSPACE_ID/EVALS_BASE_URL。
qwen-asr-evals:
	EVALS_QWEN_ASR=1 go test -count=1 -run TestLiveQwenASRThroughGatewayProxy -v ./internal/transport/httpapi/handlers/business/speech
