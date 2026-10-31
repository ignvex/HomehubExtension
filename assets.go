package main

import (
	"embed"
	"io/fs"
)

// all:web/dist 把前端构建产物打进二进制。
//
// 仓库里只提交 web/dist/.gitkeep 作为占位：go:embed 要求目录存在且
// 至少有一个匹配文件，空目录会让全新 clone 直接编译失败。
//
// `.gitkeep` 是空文件，只能满足编译，不能当页面：serve 启动时读不到
// index.html，任何页面路径都会拿到一行纯文本「前端尚未构建」
// （见 httpapi/spa.go 的 serveIndex）。要看到真实界面，先跑
// `pnpm --dir web build` 再重新编译。
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
