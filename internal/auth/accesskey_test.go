package auth

import (
	"strings"
	"testing"
)

// 规范化必须容忍用户复制时带来的各种噪声，同时不能改变语义。
func TestNormalizeAccessKey(t *testing.T) {
	// 固定一个主体，便于逐项断言。
	const body = "ABCD2345EFGH6789JKM9"

	cases := map[string]string{
		"标准形式":  "HHE-" + body,
		"小写":    "hhe-" + strings.ToLower(body),
		"无分隔符":  "HHE" + body,
		"省略前缀":  body,
		"前后空格":  "  HHE-" + body + "  ",
		"混用分隔符": "HHE " + body[:4] + "_" + body[4:],
	}
	for name, input := range cases {
		if got := normalizeAccessKey(input); got != body {
			t.Errorf("%s: normalizeAccessKey(%q) = %q, want %q", name, input, got, body)
		}
	}
}

// 主体本身可以以 HHE 开头：字母表含 H 和 E。
//
// 早期的实现无条件 TrimPrefix(raw, "HHE")，会把这类密钥的主体前三个
// 字符切掉。结果是同一把密钥"带前缀"能登录、"不带前缀"登录不了（或反过来），
// 而用户完全看不出原因。
func TestNormalizeAccessKeyKeepsAmbiguousPrefix(t *testing.T) {
	const body = "HHE2345EFGH6789JKMNP" // 20 位，且恰好以 HHE 开头

	// 带前缀：长度是 3+20=23，应当去掉前缀得到完整主体。
	if got := normalizeAccessKey("HHE-" + body); got != body {
		t.Errorf("带前缀: got %q, want %q", got, body)
	}

	// 裸主体：长度只有 20，绝不能当成"缺失前缀"去裁。
	if got := normalizeAccessKey(body); got != body {
		t.Errorf("裸主体被裁了: got %q, want %q", got, body)
	}

	// 两者必须不同——如果相同，说明其中一种被错误地规范化了。
	if normalizeAccessKey("HHE-"+body) == normalizeAccessKey(strings.TrimPrefix(body, "HHE")) {
		t.Error("带前缀与裸主体算出了同一个哈希，说明前缀处理有歧义")
	}
}

// 生成出来的密钥必须符合约定格式：前缀 + 5 组 4 位。
func TestAccessKeyPlainFormat(t *testing.T) {
	plain, err := accessKeyPlain()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plain, AccessKeyPrefix+"-") {
		t.Errorf("前缀不对: %q", plain)
	}
	body := strings.TrimPrefix(plain, AccessKeyPrefix+"-")
	parts := strings.Split(body, "-")
	if len(parts) != 5 {
		t.Fatalf("分组数 = %d, want 5: %q", len(parts), plain)
	}
	for _, p := range parts {
		if len(p) != 4 {
			t.Errorf("分组长度 = %d, want 4: %q", len(p), plain)
		}
		for _, r := range p {
			if !strings.ContainsRune(dialAlphabet, r) {
				t.Errorf("出现字符表外的字符 %q: %q", r, plain)
			}
		}
	}
}

// 两把密钥不能相同。20 位 31 进制下碰撞不现实，但生成器写错
// （比如复用缓冲）会让它们必然相同。
func TestAccessKeyPlainUnique(t *testing.T) {
	seen := make(map[string]struct{}, 64)
	for i := 0; i < 64; i++ {
		plain, err := accessKeyPlain()
		if err != nil {
			t.Fatal(err)
		}
		if _, dup := seen[plain]; dup {
			t.Fatalf("第 %d 次生成重复: %q", i, plain)
		}
		seen[plain] = struct{}{}
	}
}
