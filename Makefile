.PHONY: build build-linux test e2e run tidy vet lint docs verify qwen-asr-evals

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

# 打标签的集成 e2e(真 HTTP 栈 + 真 SQLite + 生产装配函数)。
#
# **它必须在 verify 里,因为 `go test ./...` 看不见它**——build tag 把它挡在外面,于是它可以红着过好几个
# 提交而无人知晓。实证:撤掉第二个 provider 之后整包 13 个用例全红(chat 全部解析到多模态模型,而这些栈只接了
# 文本槽),而本地四项门禁一路全绿,直到 CI 才说话。一个跑不到的测试等于没有测试。
#
# The tagged e2e MUST be in verify: `go test ./...` cannot see it (the build tag keeps it out), so it
# can stay red across commits unnoticed — as it did after the second provider was removed, when all 13 cases in
# the package failed while the local gate reported four greens.
e2e:
	go test -tags=integration -race -count=1 ./internal/e2e/...

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

# 本地门禁:vet + build + race 测试 + 打标签 e2e + lint + docs 全绿。
#
# **lint 在这里,是因为 CI 里有它。** 少了这一条,本地 verify 会对着一份比 CI 宽的规则说「全绿」,而
# 差额只在推上去之后才现形——真发生过:本地四项全过,CI 一次抓出一个真的连接泄漏(bodyclose)
# 加七处英式拼写。一个比 CI 宽松的本地门禁不是快,是**把发现时间挪到最贵的地方**。
#
# The local gate mirrors CI on purpose: without `lint` here, `make verify` says green against a
# LOOSER ruleset than CI's, and the difference only shows up after a push — which is exactly how a
# real connection leak (bodyclose) plus seven British spellings once got past a four-green local
# run. A local gate laxer than CI is not faster; it moves discovery to the most expensive place.
verify: vet build test e2e lint docs

# 显式付费 live eval：真实连接 DashScope realtime ASR，经本地 gateway speech proxy 路径完成一次会话。
# 必须由调用者提供 DASHSCOPE_API_KEY/EVALS_KEY 与 DASHSCOPE_WORKSPACE_ID/EVALS_BASE_URL。
qwen-asr-evals:
	EVALS_QWEN_ASR=1 go test -count=1 -run TestLiveQwenASRThroughGatewayProxy -v ./internal/transport/httpapi/handlers/business/speech
