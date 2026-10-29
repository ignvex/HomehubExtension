package config

import (
	"encoding/json"
	"fmt"
	"time"
)

// Duration 是一个可从 JSON 字符串解析的时长，写法如 "5s"、"30m"、"168h"。
// 用字符串而不是纳秒整数：配置是给人看的，写错时错误信息也应可读。
type Duration struct {
	time.Duration
}

// Zero 表示"未设置"，由默认值填充。
func (d Duration) IsZero() bool { return d.Duration == 0 }

// UnmarshalJSON 解析 "5s" 这类时长字符串。负数与非法格式都报错。
func (d *Duration) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		d.Duration = 0
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"5s\", got %s", string(b))
	}
	if s == "" {
		d.Duration = 0
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	if v < 0 {
		return fmt.Errorf("invalid duration %q: must not be negative", s)
	}
	d.Duration = v
	return nil
}

// MarshalJSON 输出时长字符串，与输入的写法一致。
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

// Seconds 便于写入数据库（SQLite 存整数秒）。
func (d Duration) Seconds() int64 { return int64(d.Duration / time.Second) }
