.PHONY: help dev-api dev-web build bundle standalone test race vet fmt web clean lint units sec

BINARY := dist/homehub-extension
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

build: web ## 交叉编译 linux/amd64 单二进制，并汇总分发包到 dist/
	# -a 强制重建：go:embed 的目录变化在 9p / 网络文件系统上
	# 可能检测不到 mtime 变化，导致二进制里嵌的是旧前端。
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) .
	@echo "== 汇总分发包 =="
	@# dist/ 是一个可整体拷到服务器的自包含目录：二进制 + 安装脚本
	@# + 它需要的单元与配置模板。脚本不内嵌这些内容，避免与 deploy/
	@# 下的唯一来源漂移。
	@install -m 0755 scripts/install.sh dist/install.sh
	@install -m 0755 deploy/backup.sh dist/backup.sh
	@install -m 0644 deploy/hhe.service deploy/hhe-backup.service \
	  deploy/hhe-backup.timer dist/
	@install -m 0644 config.example.json dist/config.example.json
	@ls -lh dist/
	# 顺手生成「脚本 + 自备二进制」形态：dist/hhe.sh 只有几十 KB。
	@scripts/bundle.sh "$(VERSION)" --no-binary >/dev/null
	@echo "    dist/hhe.sh 就绪（把它和你的 homehub-extension 放同一目录）"

bundle: build ## 打成单个自解压安装脚本（dist/hhe-installer-<ver>-linux-amd64.run）
	@scripts/bundle.sh "$(VERSION)"

standalone: build ## 生成 dist/hhe.sh：只需脚本 + 你自备的二进制
	@scripts/bundle.sh "$(VERSION)" --no-binary

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
	rm -rf bin/ dist/ web/dist/_app web/dist/assets

lint: ## 规范检查（Golang Specification skill）
	@echo "== gofmt =="
	@unformatted=$$(gofmt -l . 2>/dev/null | grep -v node_modules); \
	  if [ -n "$$unformatted" ]; then echo "$$unformatted"; exit 1; fi; echo "  OK"
	@echo "== go vet =="
	@go vet ./... && echo "  OK"
	@echo "== deadcode =="
	@# 不能写成 `command -v deadcode && deadcode … || echo 跳过`：那种写法下
	@# deadcode 自己失败也会走进 `||`，把真问题报成「未安装」。
	@if command -v deadcode >/dev/null; then deadcode -test ./... || exit 1; \
	  else echo "  (未安装，跳过)"; fi
	@echo "== 超长行 (>120) =="
	@for f in $$(find . -name '*.go' -not -path './web/*' -not -name '*_test.go'); do \
	  awk -v F="$$f" 'length>120 {printf "%s:%d (%d)\n", F, FNR, length}' "$$f"; done; echo "  OK"
	@echo "== 超长函数 (>80 行) =="
	@python3 scripts/check-gofunc.py
	@echo "== import 分组 =="
	@python3 scripts/check-goimports.py

sec: ## 安全扫描（gosec）
	@# G104 = 未处理错误。仓库里 24 处全是 os.Remove / db.Close
	@# 这类清理操作，失败不影响主流程，逐个判分支只会把代码变吵。
	@# 其余 medium 及以上的告警必须清零：真告警都走过审查，
	@# 在对应行用 `#nosec Gxxx -- 理由` 标出，不需要另维护白名单文件。
	@command -v gosec >/dev/null || { \
	  echo "  未安装 gosec: go install github.com/securego/gosec/v2/cmd/gosec@latest"; exit 1; }
	@gosec -quiet -severity=medium -exclude=G104 -fmt=text ./...

units: ## 校验 systemd 单元（语法 + 加固评分）
	@command -v systemd-analyze >/dev/null || { echo "  未安装 systemd-analyze，跳过"; exit 0; }
	@echo "== 单元语法 =="
	@# verify 会抱怨 ExecStart 指向的二进制在本机不存在，这是预期的：
	@# 目标环境是服务器，本地只校配置文件本身；其余任何输出都是真问题。
	@out=$$(systemd-analyze verify deploy/*.service deploy/*.timer 2>&1 \
	  | grep -v "is not executable: No such file"); \
	  if [ -n "$$out" ]; then echo "$$out"; exit 1; fi; echo "  OK"
	@echo "== 加固评分 =="
	@systemd-analyze security --offline=yes deploy/hhe.service 2>&1 \
	  | grep "Overall exposure level"
