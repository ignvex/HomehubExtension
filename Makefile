.PHONY: help dev-api dev-web build test race vet fmt web clean

BINARY := bin/homehub-extension
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.buildTime=$(DATE)

help:
	@grep -E '^[a-z-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "};{printf "  %-12s %s\n", $$1, $$2}'

dev-api: ## 后端开发服务器
	go run . serve -config config.json

dev-web: ## 前端开发服务器（Vite，代理到 8080）
	pnpm --dir web dev

build: web ## 交叉编译 linux/amd64 单二进制
	# -a 强制重建：go:embed 的目录变化在 9p / 网络文件系统上
	# 可能检测不到 mtime 变化，导致二进制里嵌的是旧前端。
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) .
	@ls -lh $(BINARY)

web: ## 构建前端
	pnpm --dir web install --frozen-lockfile
	pnpm --dir web build

test: ## 单元 + 集成测试
	go test ./...

race: ## 并发路径测试
	# paste 的 argon2 在 race 模式下要分配 ~64MB shadow 内存，整套跑会很慢，
	# 所以这里只跑真正涉及并发的用例。
	go test -race -count=1 -run "Concurrent|BurnAfterRead|SSE" ./internal/paste/ ./internal/httpapi/
	go test -race -count=1 ./internal/auth/ ./internal/rss/ ./internal/scheduler/ ./internal/app/ ./internal/store/sqlite/ ./internal/blob/

vet: ## 静态检查
	go vet ./...
	pnpm --dir web exec tsc --noEmit

backup: ## 备份到 /var/backups/homehub（需部署环境）
	./deploy/backup.sh

fmt: ## 格式化
	gofmt -l -w .

clean: ## 清理产物
	rm -rf bin/ web/dist/_app web/dist/assets

lint: ## 规范检查（Golang Specification skill）
	@echo "== gofmt =="
	@unformatted=$$(gofmt -l . 2>/dev/null | grep -v node_modules); \
	  if [ -n "$$unformatted" ]; then echo "$$unformatted"; exit 1; fi; echo "  OK"
	@echo "== go vet =="
	@go vet ./... && echo "  OK"
	@echo "== deadcode =="
	@command -v deadcode >/dev/null && deadcode -test ./... || echo "  (未安装，跳过)"
	@echo "== 超长行 (>120) =="
	@for f in $$(find . -name '*.go' -not -path './web/*' -not -name '*_test.go'); do \
	  awk -v F="$$f" 'length>120 {printf "%s:%d (%d)\n", F, FNR, length}' "$$f"; done; echo "  OK"
	@echo "== 超长函数 (>80 行) =="
	@python3 scripts/check-gofunc.py
	@echo "== import 分组 =="
	@python3 scripts/check-goimports.py
