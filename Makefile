# Levis 构建脚本
#
# 前端源码在独立仓库 levis-frontend 中，默认位于本仓库的同级目录。
# 构建产物会拷贝到 internal/web/dist，由 go:embed 嵌入二进制。

BINARY      := levis
BIN_DIR     := bin
DIST_DIR    := internal/web/dist
FRONTEND    ?= ../levis-frontend
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -s -w -X main.version=$(VERSION)

# 全程禁用 cgo：SQLite 用的是纯 Go 驱动，这样才能交叉编译出单文件二进制。
GO_ENV      := CGO_ENABLED=0

# 插件契约的代码生成工具版本。三者都是纯 Go 程序，用 go run 临时拉起，
# 不必预先安装，也不进入本模块的依赖图。
BUF_VERSION      := v1.47.2
PROTOC_GEN_GO    := v1.36.12
PROTOC_GEN_GRPC  := v1.6.2
PROTO_DIR        := internal/plugin/proto

.PHONY: all build backend frontend clean test vet fmt dev-backend dev-frontend release proto

all: build

## build: 构建前端并编译出完整二进制
build: frontend backend

## backend: 只编译后端（使用当前 dist 目录中已有的前端产物）
backend:
	@mkdir -p $(BIN_DIR)
	$(GO_ENV) go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) ./cmd/levis
	@echo "已生成 $(BIN_DIR)/$(BINARY)"

## frontend: 构建前端并拷入 dist 目录
frontend:
	@if [ ! -d "$(FRONTEND)" ]; then \
		echo "找不到前端目录 $(FRONTEND)"; \
		echo "请 clone https://github.com/SakuraOpenSource/levis-frontend 到该位置，"; \
		echo "或用 make frontend FRONTEND=/path/to/levis-frontend 指定路径。"; \
		exit 1; \
	fi
	pnpm --dir $(FRONTEND) install --frozen-lockfile
	pnpm --dir $(FRONTEND) build
	@rm -rf $(DIST_DIR)
	@mkdir -p $(DIST_DIR)
	@cp -R $(FRONTEND)/dist/. $(DIST_DIR)/
	@touch $(DIST_DIR)/.gitkeep
	@echo "前端产物已拷入 $(DIST_DIR)"

## proto: 由 plugin.proto 重新生成 Go 代码（仅在改动契约后需要执行）
##
## 生成物已入库，因此常规构建与 CI 都不需要本目标，也不需要装 protoc ——
## buf 是纯 Go 实现，连 C++ 的 protoc 都省了。
proto:
	@echo "生成插件契约代码"
	@tmp=$$(mktemp -d) && \
		GOBIN=$$tmp go install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO) && \
		GOBIN=$$tmp go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GRPC) && \
		cd $(PROTO_DIR) && PATH="$$tmp:$$PATH" go run github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION) \
			generate --template buf.gen.yaml . && \
		rm -rf $$tmp
	@gofmt -w $(PROTO_DIR)
	@echo "已更新 $(PROTO_DIR)/*.pb.go，记得一并提交"

## test: 运行后端测试
test:
	$(GO_ENV) go test ./...

## vet: 静态检查
vet:
	$(GO_ENV) go vet ./...

## fmt: 格式化代码
fmt:
	gofmt -w .

## dev-backend: 以调试模式启动后端（默认监听 :8080）
dev-backend:
	$(GO_ENV) go run ./cmd/levis -debug

## dev-frontend: 启动前端开发服务器（已配置 /api 代理到 :8080）
dev-frontend:
	pnpm --dir $(FRONTEND) dev

## release: 交叉编译多平台产物到 bin/release
release: frontend
	@mkdir -p $(BIN_DIR)/release
	@for target in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64; do \
		os=$${target%/*}; arch=$${target#*/}; \
		out=$(BIN_DIR)/release/$(BINARY)-$$os-$$arch; \
		if [ "$$os" = "windows" ]; then out=$$out.exe; fi; \
		echo "构建 $$os/$$arch"; \
		$(GO_ENV) GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o $$out ./cmd/levis || exit 1; \
	done
	@echo "全部产物位于 $(BIN_DIR)/release"

## clean: 清理构建产物
clean:
	rm -rf $(BIN_DIR)
	rm -rf $(DIST_DIR)
	@mkdir -p $(DIST_DIR)
	@touch $(DIST_DIR)/.gitkeep