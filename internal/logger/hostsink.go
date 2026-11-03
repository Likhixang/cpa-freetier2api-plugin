package logger

// 本文件把插件日志镜像进宿主的日志文件（CLIProxyAPI 的 main.log）。
//
// 为什么需要：宿主管理面板的日志页只读 <auth-dir>/logs/main.log，插件的 stderr
// 输出进的是 journald，面板里看不到任何插件行为，排查只能登录机器翻 journal。
//
// 三条约束决定了这里的实现形态：
//   - 文件归宿主所有：不存在时绝不创建，否则面板会读到一份插件造的、与宿主
//     日志无关的空文件；宿主轮转后旧 fd 写的是已 rename 的 inode，因此要按
//     inode 检测重开，但每次都 stat 没必要，5 秒节流一次；
//   - 日志写不进去不能反噬主流程：任何失败都静默降级，只在 stderr 提示一次；
//   - 不能阻塞调用线程：日志来自请求路径，投递进有界队列由后台 goroutine 落盘，
//     队列满就丢弃并计数——丢日志远好于拖慢一次对话。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// hostRecheckInterval 是「文件是否还在 / 是否换了 inode」的检查节流间隔。
	hostRecheckInterval = 5 * time.Second
	// hostQueueCapacity 是待落盘日志的缓冲深度。
	hostQueueCapacity = 256
	// hostSource 是写入宿主日志的来源标识：面板按这一段区分插件日志。
	hostSource = "freetier2api"
	// hostTimeLayout 与宿主日志行的时间戳格式一致（毫秒）。
	hostTimeLayout = "2006-01-02 15:04:05.000"
)

// HostSinkOptions 描述宿主日志镜像的开关。
type HostSinkOptions struct {
	// Enabled 为 false 时关闭镜像并停掉后台 goroutine。
	Enabled bool
	// Path 是显式指定的宿主日志文件；留空时按 StateDir 自动探测。
	Path string
	// StateDir 是插件状态目录，用于反推宿主的 auth-dir。
	StateDir string
	// MinLevel 是本 sink 自己的级别门槛（host_log_level），与插件日志级别独立：
	// 插件开 debug 而镜像只要 info 时，debug 行不会进宿主日志。
	MinLevel Level
}

// hostRecord 是一条待落盘的日志。
type hostRecord struct {
	at      time.Time
	lvl     Level
	label   string
	message string
}

// hostSink 持有队列与后台 worker 的句柄。
//
// 文件句柄、inode 检查时间等可变状态全部由 worker goroutine 私有（见
// hostFileState），因此写日志这条路径不需要加锁。
type hostSink struct {
	mu    sync.Mutex
	queue chan hostRecord
	done  chan struct{}
	wg    sync.WaitGroup

	dropped atomic.Uint64
}

var hostLog = &hostSink{}

// InitHostSink 开启或关闭宿主日志镜像。可重复调用（每次注册/重载配置都会走）。
func InitHostSink(opts HostSinkOptions) {
	hostLog.stop()

	candidates := hostLogCandidates(opts)
	if !opts.Enabled || len(candidates) == 0 {
		return
	}
	minLevel := opts.MinLevel

	hostLog.mu.Lock()
	defer hostLog.mu.Unlock()
	queue := make(chan hostRecord, hostQueueCapacity)
	done := make(chan struct{})
	hostLog.queue = queue
	hostLog.done = done
	hostLog.dropped.Store(0)
	hostLog.wg.Add(1)
	go hostLog.run(queue, done, candidates, minLevel)
}

// CloseHostSink 停掉 worker 并把已入队的日志写完。供 shutdown 调用。
func CloseHostSink() { hostLog.stop() }

// stop 幂等地停掉当前 worker：先摘掉队列（此后 enqueue 直接丢弃），再等 worker
// 把残留记录写完并关闭文件。
func (s *hostSink) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done == nil {
		return
	}
	close(s.done)
	s.wg.Wait()
	s.queue = nil
	s.done = nil
}

// enqueue 把一条日志投递给后台 worker。队列满即丢弃。
func (s *hostSink) enqueue(rec hostRecord) {
	s.mu.Lock()
	queue := s.queue
	s.mu.Unlock()
	if queue == nil {
		return
	}
	select {
	case queue <- rec:
	default:
		// 只提示第一次：日志风暴时刷屏本身就违反了这个 sink 的初衷。
		if n := s.dropped.Add(1); n == 1 {
			fmt.Fprintf(os.Stderr, "[%s] host log queue is full, dropping plugin log lines\n", hostSource)
		}
	}
}

// run 是唯一的落盘方。
func (s *hostSink) run(records <-chan hostRecord, done <-chan struct{}, candidates []string, minLevel Level) {
	defer s.wg.Done()
	state := &hostFileState{}
	defer state.close()

	for {
		select {
		case <-done:
			// 收尾：把已入队的记录写完，避免关停时丢掉最后几条。
			for {
				select {
				case rec := <-records:
					state.write(rec, candidates, minLevel)
				default:
					return
				}
			}
		case rec := <-records:
			state.write(rec, candidates, minLevel)
		}
	}
}

// hostFileState 是 worker 私有的文件状态。
type hostFileState struct {
	file      *os.File
	path      string
	lastCheck time.Time
}

func (s *hostFileState) write(rec hostRecord, candidates []string, minLevel Level) {
	if rec.lvl < minLevel {
		return
	}
	file := s.target(rec.at, candidates)
	if file == nil {
		return
	}
	if _, errWrite := file.WriteString(formatHostLine(rec.at, rec.label, rec.message)); errWrite != nil {
		// 写失败就地降级：丢掉 fd，下一条日志再重新探测（宿主可能正在轮转）。
		s.close()
	}
}

// target 返回可写的宿主日志文件，顺带处理轮转。不可用时返回 nil。
func (s *hostFileState) target(now time.Time, candidates []string) *os.File {
	if now.Sub(s.lastCheck) < hostRecheckInterval {
		return s.file
	}
	s.lastCheck = now

	if s.file != nil {
		if current, errStat := os.Stat(s.path); errStat == nil {
			if opened, errOpened := s.file.Stat(); errOpened == nil && os.SameFile(opened, current) {
				return s.file
			}
		}
		s.close()
	}
	for _, candidate := range candidates {
		file, errOpen := openHostLog(candidate)
		if errOpen != nil {
			continue
		}
		s.file = file
		s.path = candidate
		return file
	}
	return nil
}

func (s *hostFileState) close() {
	if s.file != nil {
		_ = s.file.Close()
		s.file = nil
		s.path = ""
	}
}

// openHostLog 以只追加方式打开已存在的宿主日志。
//
// 刻意不带 O_CREATE：文件不存在说明宿主没把日志落到磁盘（或还没起来），
// 这时造一个空文件只会让面板显示一份没有内容的日志。
func openHostLog(path string) (*os.File, error) {
	info, errStat := os.Stat(path)
	if errStat != nil {
		return nil, errStat
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory", path)
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
}

// hostLogCandidates 返回宿主 main.log 的候选路径，按优先级排列。
//
// 插件拿不到宿主的 auth-dir，只能从自己的状态目录反推（状态目录通常落在
// <auth-dir>/<plugin>）：
//
//	<base>/auths/logs/main.log   auth-dir 是 <base>/auths
//	<base>/logs/main.log         auth-dir 就是 <base>
func hostLogCandidates(opts HostSinkOptions) []string {
	if explicit := strings.TrimSpace(opts.Path); explicit != "" {
		return []string{explicit}
	}
	stateDir := strings.TrimRight(strings.TrimSpace(opts.StateDir), string(filepath.Separator))
	if stateDir == "" {
		return nil
	}
	base := filepath.Dir(stateDir)
	if base == "" || base == "." || base == string(filepath.Separator) {
		return nil
	}
	return []string{
		filepath.Join(base, "auths", "logs", "main.log"),
		filepath.Join(base, "logs", "main.log"),
	}
}

// formatHostLine 按宿主 logger 的行格式拼一条：
//
//	[时间戳] [请求 ID] [级别] [来源] 正文
//
// 请求 ID 段始终保留（插件日志不在某个请求上下文里，这一格为空）。不省略是
// 因为面板的日志解析按固定段数取字段，段数随行变化会让整行退化成无结构文本。
func formatHostLine(now time.Time, label, message string) string {
	return fmt.Sprintf("[%s] [%s] [%s] [%s] %s\n",
		now.Format(hostTimeLayout), "", label, hostSource, oneLine(message))
}

// oneLine 把正文压成单行：宿主按行切分日志，正文里的换行会造出没有时间戳的
// 伪日志行，面板解析时要么丢弃要么错位。
func oneLine(message string) string {
	if !strings.ContainsAny(message, "\r\n") {
		return message
	}
	replaced := strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(message)
	return strings.TrimSpace(replaced)
}
