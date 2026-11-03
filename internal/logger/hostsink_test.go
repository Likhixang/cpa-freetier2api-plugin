package logger

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// hostLinePattern 只校验行结构（时间戳、请求 ID、级别、来源、正文），
// 不锁定任何具体文案。
var hostLinePattern = regexp.MustCompile(
	`^\[(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.\d{3})\] \[([^\]]*)\] \[([A-Z]+)\] \[freetier2api\] (.+)$`)

// setupHostLog 在临时目录里造出 <base>/auths/logs/main.log，并让状态目录落在
// <base>/freetier2api（自动探测就是从状态目录的父目录反推的）。
func setupHostLog(t *testing.T) (stateDir, logFile string) {
	t.Helper()
	base := t.TempDir()
	logFile = filepath.Join(base, "auths", "logs", "main.log")
	if errMkdir := os.MkdirAll(filepath.Dir(logFile), 0o755); errMkdir != nil {
		t.Fatalf("create log dir: %v", errMkdir)
	}
	if errWrite := os.WriteFile(logFile, nil, 0o644); errWrite != nil {
		t.Fatalf("create host log: %v", errWrite)
	}
	stateDir = filepath.Join(base, "freetier2api")
	if errMkdir := os.MkdirAll(stateDir, 0o700); errMkdir != nil {
		t.Fatalf("create state dir: %v", errMkdir)
	}
	return stateDir, logFile
}

func readHostLines(t *testing.T, path string) []string {
	t.Helper()
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read host log: %v", errRead)
	}
	trimmed := strings.TrimRight(string(raw), "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

func TestHostSinkAppendsHostFormattedLines(t *testing.T) {
	stateDir, logFile := setupHostLog(t)
	// 插件日志开到 debug，但镜像只要 info：debug 行必须被 sink 自己挡掉。
	SetLevel("debug")
	defer SetLevel("info")
	InitHostSink(HostSinkOptions{Enabled: true, StateDir: stateDir, MinLevel: LevelInfo})
	// 顺序敏感：Close 会把队列里剩下的记录写完，之后才读文件。
	Info("first mirrored line")
	Error("second mirrored line, %d", 42)
	Debug("debug is below the sink threshold")
	CloseHostSink()

	lines := readHostLines(t, logFile)
	if len(lines) != 2 {
		t.Fatalf("host log lines = %d (%q), want 2", len(lines), lines)
	}
	want := []struct {
		level   string
		message string
	}{
		{"INFO", "first mirrored line"},
		{"ERROR", "second mirrored line, 42"},
	}
	for i, line := range lines {
		match := hostLinePattern.FindStringSubmatch(line)
		if match == nil {
			t.Fatalf("line %d %q does not match the host log format", i, line)
		}
		if match[3] != want[i].level {
			t.Errorf("line %d level = %q, want %q", i, match[3], want[i].level)
		}
		if match[4] != want[i].message {
			t.Errorf("line %d message = %q, want %q", i, match[4], want[i].message)
		}
		if _, errParse := time.ParseInLocation(hostTimeLayout, match[1], time.Local); errParse != nil {
			t.Errorf("line %d timestamp %q is not parseable: %v", i, match[1], errParse)
		}
	}
}

func TestHostSinkNeverCreatesMissingFile(t *testing.T) {
	base := t.TempDir()
	stateDir := filepath.Join(base, "freetier2api")
	logFile := filepath.Join(base, "logs", "main.log")

	InitHostSink(HostSinkOptions{Enabled: true, StateDir: stateDir, MinLevel: LevelInfo})
	Info("host has not opened its log file yet")
	CloseHostSink()

	if _, errStat := os.Stat(logFile); !os.IsNotExist(errStat) {
		t.Fatalf("plugin must not create %s (stat err = %v)", logFile, errStat)
	}
}

func TestHostSinkExplicitPathWins(t *testing.T) {
	base := t.TempDir()
	explicit := filepath.Join(base, "custom-main.log")
	if errWrite := os.WriteFile(explicit, nil, 0o644); errWrite != nil {
		t.Fatalf("create host log: %v", errWrite)
	}
	InitHostSink(HostSinkOptions{
		Enabled:  true,
		Path:     explicit,
		StateDir: filepath.Join(base, "freetier2api"),
		MinLevel: LevelInfo,
	})
	Info("explicit path line")
	CloseHostSink()

	lines := readHostLines(t, explicit)
	if len(lines) != 1 {
		t.Fatalf("host log lines = %d (%q), want 1", len(lines), lines)
	}
	if !strings.Contains(lines[0], "explicit path line") {
		t.Fatalf("line = %q", lines[0])
	}
}

func TestFormatHostLineKeepsOneLinePerRecord(t *testing.T) {
	now := time.Date(2026, 11, 3, 10, 0, 0, 0, time.Local)
	line := formatHostLine(now, "INFO", "first line\nsecond line")
	if strings.Count(line, "\n") != 1 || !strings.HasSuffix(line, "\n") {
		t.Fatalf("line = %q, want exactly one trailing newline", line)
	}
	match := hostLinePattern.FindStringSubmatch(strings.TrimSuffix(line, "\n"))
	if match == nil {
		t.Fatalf("line %q does not match the host log format", line)
	}
	if !strings.Contains(match[4], "first line second line") {
		t.Fatalf("message = %q", match[4])
	}
}
