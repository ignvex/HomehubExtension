package main

import (
	"embed"
	"io/fs"
)

// all:web/dist 把前端构建产物打进二进制。
//
// 仓库里提交了 web/dist/index.html 作为占位，保证全新 clone 直接
// go build 不会失败；`pnpm --dir web build` 会用真实产物覆盖它。
//
//go:embed all:web/dist
var webFS embed.FS

// webAssets 返回以 web/dist 为根的子树。
func webAssets() fs.FS {
	sub, err := fs.Sub(webFS, "web/dist")
	if err != nil {
		// 只有 embed 指令写错才会走到这里，属于编译期问题。
		return nil
	}
	return sub
}
