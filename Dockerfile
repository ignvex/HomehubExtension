# syntax=docker/dockerfile:1

# 多阶段构建。
#
# 关键约束：前端必须在 Go 编译之前构建完毕。
# //go:embed all:web/dist 在编译期读取文件，顺序错了会产出一个
# 只有占位页的二进制——体积正常、能启动、就是没有界面。

ARG GO_VERSION=1.27
ARG NODE_VERSION=22

# ---- 前端 ----
FROM node:${NODE_VERSION}-alpine AS web
WORKDIR /src/web

# 先只拷锁文件，让依赖层能够被缓存。
COPY web/package.json web/pnpm-lock.yaml web/pnpm-workspace.yaml ./
RUN corepack enable && pnpm install --frozen-lockfile

COPY web/ ./
RUN pnpm run build

# ---- 后端 ----
FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# 覆盖掉版本库里的占位 dist。
COPY --from=web /src/web/dist ./web/dist

ARG VERSION=dev
ARG COMMIT=none
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w \
        -X main.version=${VERSION} \
        -X main.commit=${COMMIT} \
        -X main.buildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
      -o /out/homehub-extension .

# ---- 运行 ----
FROM alpine:3.21

# tzdata 用于 cleanup.timezone；ca-certificates 用于抓取 HTTPS 订阅源。
RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -S -g 10001 homehub \
 && adduser  -S -u 10001 -G homehub -h /var/lib/homehub-extension homehub \
 && mkdir -p /var/lib/homehub-extension/blobs /etc/homehub-extension \
 && chown -R homehub:homehub /var/lib/homehub-extension

COPY --from=build /out/homehub-extension /usr/local/bin/homehub-extension

USER homehub
WORKDIR /var/lib/homehub-extension

# 容器内必须监听 0.0.0.0 才能被宿主或同网络的反代访问，
# 所以这里显式要求配置里设 allow_remote_listen=true。
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/homehub-extension"]
CMD ["serve", "-config", "/etc/homehub-extension/config.json"]
