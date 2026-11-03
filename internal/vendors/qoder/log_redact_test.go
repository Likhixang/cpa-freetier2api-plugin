package qoder

import (
	"encoding/json"
	"strings"
	"testing"

	"freetier2api-plugin/internal/logger"
)

const secretPrompt = "请帮我写一封辞职信，收件人是张三，理由是薪资太低。SECRET-TOKEN-123"

func sampleQoderBody() map[string]interface{} {
	return map[string]interface{}{
		"model_config": map[string]interface{}{"key": "auto", "source": "system"},
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": "be brief"},
			map[string]interface{}{"role": "user", "content": []interface{}{
				map[string]interface{}{"type": "text", "text": secretPrompt},
				map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "data:image/png;base64," + strings.Repeat("A", 64)}},
			}},
		},
		"tools": []interface{}{map[string]interface{}{"name": "lookup", "description": "hi"}},
	}
}

// captureLogs 在指定级别下执行 fn，返回期间新增的日志。
func captureLogs(t *testing.T, level string, fn func()) []logger.Entry {
	t.Helper()
	previous := logger.CurrentLevel()
	logger.SetLevel(level)
	defer logger.SetLevel(previous)
	_, since := logger.Snapshot(0, 1)
	fn()
	entries, _ := logger.Snapshot(since, 100)
	return entries
}

func TestLogQoderRequestInfoHasNoConversationContent(t *testing.T) {
	body := sampleQoderBody()
	entries := captureLogs(t, "info", func() { logQoderRequest("auto", secretPrompt, 2, body) })
	if len(entries) != 1 {
		t.Fatalf("info level should log exactly one metadata line, got %d: %+v", len(entries), entries)
	}
	message := entries[0].Message
	if strings.Contains(message, "辞职信") || strings.Contains(message, "SECRET") {
		t.Fatalf("info log leaked conversation content: %q", message)
	}
	// 字符数按 rune 计，不是字节：中文不会被截断或多算。
	want := "callQoder model=auto messages=2 prompt_chars=" + itoa(len([]rune(secretPrompt)))
	if message != want {
		t.Fatalf("info log = %q, want %q", message, want)
	}
}

func TestLogQoderRequestDebugRedactsBodyWithoutMutatingIt(t *testing.T) {
	body := sampleQoderBody()
	before, _ := json.Marshal(body)
	entries := captureLogs(t, "debug", func() { logQoderRequest("auto", secretPrompt, 2, body) })

	var debugLine string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Message, "callQoder request body: ") {
			debugLine = entry.Message
		}
	}
	if debugLine == "" {
		t.Fatalf("debug level should log the request body, got %+v", entries)
	}
	for _, leaked := range []string{"辞职信", "SECRET", "be brief", "base64,AAAA"} {
		if strings.Contains(debugLine, leaked) {
			t.Fatalf("debug body leaked %q: %s", leaked, debugLine)
		}
	}
	// 非内容字段与短类型标记保留，便于排查。
	for _, kept := range []string{`"role":"user"`, `"type":"text"`, `"type":"image_url"`, `"source":"system"`, `"name":"lookup"`, "<redacted len="} {
		if !strings.Contains(debugLine, kept) {
			t.Fatalf("debug body should keep %s: %s", kept, debugLine)
		}
	}
	after, _ := json.Marshal(body)
	if string(before) != string(after) {
		t.Fatalf("logging must not mutate the outgoing body:\nbefore %s\nafter  %s", before, after)
	}
}

func itoa(value int) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}
