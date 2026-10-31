// Package systemd 实现 sd_notify(3) 协议的一个子集。
//
// 为什么自己写而不是引依赖：协议就是「往 $NOTIFY_SOCKET 写一行 KEY=VALUE」，
// 几十行足够；而引一个库会给 CGO_ENABLED=0 的静态构建带来没必要的风险。
//
// 非 systemd 环境下所有函数都是 no-op：$NOTIFY_SOCKET 不存在就直接返回。
// 这意味着同一个二进制既能被 systemd 拉起，也能在开发机上裸跑，
// 不需要额外的开关或命令行参数。
package systemd

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// notifySocket 返回 $NOTIFY_SOCKET 对应的可用地址。
//
// systemd 用首字符 '@' 表示抽象命名空间，而 Go 的 net 包用 "\x00" 前缀
// 表达同一件事。漏掉这层转换的表现是：环境里明明有 NOTIFY_SOCKET，
// 连接却报 "no such file or directory"。
func notifySocket() (string, bool) {
	addr := os.Getenv("NOTIFY_SOCKET")
	if addr == "" {
		return "", false
	}
	if addr[0] == '@' {
		return "\x00" + addr[1:], true
	}
	return addr, true
}

// Notify 向 systemd 发送一条状态消息，格式为若干行 KEY=VALUE。
//
// 不在 systemd 下运行时返回 nil：调用方不需要区分这两种情况。
func Notify(state string) error {
	addr, ok := notifySocket()
	if !ok {
		return nil
	}
	// SOCK_DGRAM 且发送方无需 bind：systemd 只读数据报内容。
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: addr, Net: "unixgram"})
	if err != nil {
		return fmt.Errorf("dial notify socket: %w", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte(state)); err != nil {
		return fmt.Errorf("write notify socket: %w", err)
	}
	return nil
}

// Ready 宣告服务已可用。
//
// 只在监听真正 bind 成功、迁移已完成后调用。systemd 的 Type=notify 会
// 一直等到这条消息，所以它的语义是「现在可以接流量了」，
// 而不是「进程已经起来了」。
func Ready() error { return Notify("READY=1") }

// Status 更新 systemd 侧的一行状态，会出现在 `systemctl status` 里。
func Status(format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	// 换行会破坏 KEY=VALUE 的行结构，压成空格。
	msg = strings.NewReplacer("\r", " ", "\n", " ").Replace(msg)
	return Notify("STATUS=" + msg)
}

// Stopping 宣告正在关闭。之后 systemd 不会再把这个阶段的退出当作失败。
func Stopping() error { return Notify("STOPPING=1") }

// WatchdogInterval 返回 systemd 配置的心跳周期；未启用时返回 0。
func WatchdogInterval() time.Duration {
	raw := os.Getenv("WATCHDOG_USEC")
	if raw == "" {
		return 0
	}
	usec, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || usec <= 0 {
		return 0
	}
	return time.Duration(usec) * time.Microsecond
}

// KeepWatchdog 按 WATCHDOG_USEC 的一半周期发送心跳，直到 ctx 结束。
//
// 它证明的只是「进程还能被调度」——goroutine 池没被死锁锁住。这恰好是
// systemd 侧最有价值的一类信号：没有它，一个卡死的进程会一直占着端口，
// 反代照常把请求送进去，而 systemd 认为一切正常。
//
// 更深的健康检查（DB 可写、blob 目录可写）交给 /healthz，不要塞进心跳：
// 心跳失败会被 systemd 直接 SIGKILL，不适合承载有副作用的探测。
func KeepWatchdog(ctx context.Context, log *slog.Logger) {
	interval := WatchdogInterval()
	if interval <= 0 {
		return
	}
	// 官方建议发送间隔不超过 WatchdogSec 的一半，留足调度抖动余量。
	tick := time.NewTicker(interval / 2)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := Notify("WATCHDOG=1"); err != nil {
				log.Warn("sd_notify watchdog failed", "err", err)
			}
		}
	}
}

// Enabled 报告当前进程是否由 systemd 的 notify 方式拉起。
func Enabled() bool {
	_, ok := notifySocket()
	return ok
}
