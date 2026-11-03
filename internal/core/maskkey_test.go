package core

import (
	"math/rand"
	"strings"
	"testing"
)

// legacyMaskedKey 是合并前 admin.maskedAPIKey 与 opencodezen.MaskedKey 的实现（两者
// 逐字相同），原样保留作对照：MaskKey 的结果参与生成凭证文件名，输出必须逐字不变，
// 否则同一个 key 会生成新文件名、旧凭证文件对不上。
func legacyMaskedKey(key string) string {
	trimmed := strings.TrimSpace(key)
	if len(trimmed) <= 8 {
		if trimmed == "" {
			return ""
		}
		return "***"
	}
	return trimmed[:4] + "***" + trimmed[len(trimmed)-4:]
}

func TestMaskKeyCases(t *testing.T) {
	cases := map[string]string{
		"":                     "",
		"   ":                  "",
		"abc":                  "***",
		"12345678":             "***",
		"123456789":            "1234***6789",
		"abcdefghijkl":         "abcd***ijkl",
		"  sk-1234567890abcd ": "sk-1***abcd",
	}
	for input, want := range cases {
		if got := MaskKey(input); got != want {
			t.Fatalf("MaskKey(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestMaskKeyMatchesLegacyOutput 用固定种子生成 5000 个各种长度、含空白与多字节
// 字符的 key，逐个比对新旧实现。
func TestMaskKeyMatchesLegacyOutput(t *testing.T) {
	alphabet := []rune("abcXYZ0189-_.=/+ \t\n长键🔑é")
	random := rand.New(rand.NewSource(20261103))
	for index := 0; index < 5000; index++ {
		length := random.Intn(40)
		var builder strings.Builder
		for position := 0; position < length; position++ {
			builder.WriteRune(alphabet[random.Intn(len(alphabet))])
		}
		key := builder.String()
		if got, want := MaskKey(key), legacyMaskedKey(key); got != want {
			t.Fatalf("MaskKey(%q) = %q, legacy %q", key, got, want)
		}
	}
}
