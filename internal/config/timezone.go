package config

import "time"

// loadTimezone 独立出来便于测试替换，也提醒实现者：
// 二进制必须 import _ "time/tzdata"，否则精简系统上 Asia/Shanghai 会解析失败。
func loadTimezone(name string) (*time.Location, error) {
	return time.LoadLocation(name)
}
