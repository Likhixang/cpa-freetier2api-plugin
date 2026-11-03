package raccoon

// 本文件实现小浣熊的网页授权登录（/code/authorize 授权页 + loopback 回调）。
//
// 流程（与 Agent2API 的网页授权登录一致）：
//
//  1. LoginStart 生成一次性 state（64 位小写 hex），在本机起一次性回调监听，
//     返回官方授权页 URL：
//     {origin}/code/authorize?login_source=desktop&appname=…&state=…&
//     desktop_callback=loopback&redirect=http://127.0.0.1:<port>/auth/callback
//  2. 用户在官方页面登录（微信/验证码/密码均可）；
//  3. 授权页按 redirect 回跳本机 /auth/callback?code=…&state=…，监听收到 code
//     当场换票，登录自动完成（同机场景）；
//  4. 插件与浏览器不同机时那条 HTTP 回调打不到这里，地址栏里仍留着整条链接，
//     用户把它粘回控制台，SubmitCallback 解析后走同一条换票路径。
//
// 为什么强制 loopback：授权页只放行 office-raccoon:// 深链或 127.0.0.1 回调，
// 而深链会被已安装的官方客户端截走，插件永远拿不到 code，页面一直停在
// 「等待用户完成登录」。
//
// 说明：panel 项目早期的微信扫码通道（/login/mp 承载页 +
// login_with_qrcode_code 轮询）已随上游页面下线而不可用（实测 404，
// 2026-10-24），因此本实现只保留授权页通道。
//
// state 的安全语义（Agent2API 同口径）：由插件生成、登记进共享会话表
// （带 TTL 与轮询节流）；回调里的 state 必须逐字相等才消费，一次性。

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

// loginAppName 是授权页上报的客户端名（与官方桌面客户端一致）。
const loginAppName = "办公小浣熊客户端"

// loginCallbackScheme / Host / Path 是官方客户端回调的固定形状。
const (
	loginCallbackScheme = "office-raccoon"
	loginCallbackHost   = "auth"
	loginCallbackPath   = "/callback"
)

// loopback 回调的固定形状。授权页对 desktop_callback=loopback 只放行
// http://127.0.0.1:<port>/auth/callback（路径逐字校验），端口只要求落在
// 1024-65535，因此可以按实际绑定的端口拼 redirect。
const (
	// CallbackPort 是首选回调端口：固定端口便于用户辨认，也便于排查占用。
	CallbackPort = 37913
	// CallbackPath 是回调路径（授权页硬编码，不能改）。
	CallbackPath = "/auth/callback"
	// callbackHost 是回环地址：监听只绑本机，不对外暴露。
	callbackHost = "127.0.0.1"
)

// qrLoginTTL / qrPollInterval 是登录会话有效期与轮询节流。
const (
	qrLoginTTL     = 15 * time.Minute
	qrPollInterval = 2 * time.Second
)

// PendingLogin 是一次进行中的登录会话数据。
//
// TTL 与轮询节流由 internal/auth 的会话表统一管理，这里只保留协议状态。
type PendingLogin struct {
	State string

	// mu 保护 result：写入方是 loopback 回调的 HTTP handler（浏览器线程），
	// 读取方是控制台的 LoginPoll（宿主线程），两者必然并发。
	mu sync.Mutex
	// result 是回调监听自动换到的凭证，等下一次 LoginPoll 取走并交给宿主落盘。
	//
	// 为什么不在回调里直接删会话：凭证的落盘由宿主用 LoginPoll 返回的
	// AuthData 完成，回调线程拿到的结果没有出口。删掉会话会让「页面上显示
	// 登录成功、账号库里却没有这个账号」，所以结果必须留在会话里等轮询来取。
	result *core.CallbackSubmitResult
}

// StoreResult 记录一次自动完成的登录结果（供轮询取走）。
func (p *PendingLogin) StoreResult(result *core.CallbackSubmitResult) {
	if p == nil || result == nil {
		return
	}
	p.mu.Lock()
	p.result = result
	p.mu.Unlock()
}

// TakeResult 取走已完成的登录结果（一次性）。
func (p *PendingLogin) TakeResult() *core.CallbackSubmitResult {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	result := p.result
	p.result = nil
	return result
}

// StoreLoginSession 登记一次进行中的登录会话。
func StoreLoginSession(logins core.LoginSessions, sessionID string, session *PendingLogin) {
	if session == nil {
		return
	}
	logins.Register(VendorID, sessionID, qrLoginTTL, qrPollInterval, session)
}

// RemoveLoginSession 登录成功或终止后清理会话。
func RemoveLoginSession(logins core.LoginSessions, sessionID string) {
	logins.Delete(sessionID)
	stopCallbackCatcher(sessionID)
}

// CheckLoginSession 检查登录会话状态与节流。
// 返回 (session, exists, shouldPoll)。
func CheckLoginSession(logins core.LoginSessions, sessionID string) (*PendingLogin, bool, bool) {
	entry, pollState := logins.Poll(sessionID)
	if pollState == core.PollMissing {
		return nil, false, false
	}
	pending, okPending := entry.Data.(*PendingLogin)
	if !okPending {
		return nil, false, false
	}
	return pending, true, pollState == core.PollReady
}

// pendingMessage 是轮询等待时的进度文案。
//
// 必须把两条路径都说清：同机时浏览器会自己回跳本机端口、登录当场完成；
// 不同机时那条 HTTP 回调永远打不到插件，用户得把地址栏里的整条链接粘回来。
// 只说其中一条，用户就会一直盯着「等待用户完成登录」不知道下一步做什么。
func pendingMessage(redirect string, listening bool) string {
	if !listening {
		return "等待用户在授权页完成登录…本机回调端口不可用，登录无法自动完成；" +
			"请在授权后把浏览器地址栏里的整条链接（含 code 与 state）复制回控制台提交"
	}
	return "等待用户在授权页完成登录…确认授权后浏览器会回跳本机 " + redirect +
		"：插件与浏览器在同一台机器时自动完成登录；浏览器在另一台机器时，把地址栏里的整条链接（含 code 与 state）复制回控制台提交"
}

// CheckCallbackState 校验回调链接里的 state 与当前会话一致（逐字比对）。
func CheckCallbackState(session *PendingLogin, callbackState string) bool {
	return session != nil && strings.TrimSpace(callbackState) == session.State
}

// ErrCodeExpired 是「授权码已失效」的上游业务码（200035）：不是登录服务
// 坏了，是用户把回调链接放太久或重复点击，文案必须提示重新发起。
const ErrCodeExpired = 200035

// ExchangeAuthorizationCode 用一次性授权码换凭证
// （POST {auth_base}/login_with_authorization_code）。
func ExchangeAuthorizationCode(ctx context.Context, authBase, code string) (access, refresh string, expiresAt int64, uid string, err error) {
	if strings.TrimSpace(authBase) == "" {
		return "", "", 0, "", fmt.Errorf("upstream auth base URL is not configured")
	}
	body, errMarshal := json.Marshal(map[string]string{"authorization_code": code})
	if errMarshal != nil {
		return "", "", 0, "", fmt.Errorf("marshal exchange request: %w", errMarshal)
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, authBase+LoginCodePath, strings.NewReader(string(body)))
	if errReq != nil {
		return "", "", 0, "", fmt.Errorf("build exchange request: %w", errReq)
	}
	ApplyAPIHeaders(req, "")
	req.Header.Set("Content-Type", "application/json")

	resp, errDo := hostbridge.Client(ctx, apiHTTPTimeout).Do(req)
	if errDo != nil {
		return "", "", 0, "", &core.Failure{Msg: "exchange: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if errRead != nil {
		return "", "", 0, "", &core.Failure{Status: resp.StatusCode, Msg: "read exchange response: " + errRead.Error(), Kind: core.FailureKindTransient}
	}
	var envelope struct {
		Code    *int64          `json:"code"`
		Message string          `json:"message"`
		Msg     string          `json:"msg"`
		Details string          `json:"details"`
		Data    json.RawMessage `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		return "", "", 0, "", Classify(resp.StatusCode, string(raw))
	}
	errMsg := core.FirstNonEmpty(envelope.Message, envelope.Msg, envelope.Details)
	if envelope.Code != nil {
		if *envelope.Code == ErrCodeExpired || strings.Contains(errMsg, "authorization_code_not_found") {
			return "", "", 0, "", &core.Failure{Status: resp.StatusCode, Msg: "授权码已失效或不存在，请重新发起登录", Kind: core.FailureKindClient}
		}
		if *envelope.Code != 0 {
			return "", "", 0, "", ClassifyBusiness(resp.StatusCode, *envelope.Code, errMsg)
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", 0, "", Classify(resp.StatusCode, string(raw))
	}
	var data struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresAt    any    `json:"expires_at"`
		UserID       string `json:"user_id"`
	}
	if len(envelope.Data) > 0 && string(envelope.Data) != "null" {
		if errUnmarshal := json.Unmarshal(envelope.Data, &data); errUnmarshal != nil {
			return "", "", 0, "", fmt.Errorf("parse exchange data: %w", errUnmarshal)
		}
	}
	access = strings.TrimSpace(data.AccessToken)
	refresh = strings.TrimSpace(data.RefreshToken)
	if access == "" || refresh == "" {
		return "", "", 0, "", &core.Failure{Status: resp.StatusCode, Msg: "小浣熊未返回完整凭证（access_token/refresh_token）", Kind: core.FailureKindServer}
	}
	if ms := core.GetUnixSeconds(map[string]any{"expires_at": data.ExpiresAt}, "expires_at"); ms > 0 {
		expiresAt = ms
	}
	uid = strings.TrimSpace(data.UserID)
	return access, refresh, expiresAt, uid, nil
}

// ParseCallbackCode 从回调链接里取授权码与 state。
//
// 接受两种形态：
//   - 深链 office-raccoon://auth/callback?code=…&state=…（官方客户端注册的协议）；
//   - loopback http://127.0.0.1:<port>/auth/callback?code=…&state=…（授权页
//     按 desktop_callback=loopback 回跳的那条）。
//
// 两种都逐项比对 scheme/host/path，不用前缀匹配：office-raccoon://auth/callback@evil.com
// 这类形态用 startsWith 会全部放行。回调落在自定义协议与回环端口上，任何本机
// 程序都能构造一条丢进登录窗口，不校验 state 就等于把「用别人的 code 换来的
// 凭证写进你的账号库」敞开。
//
// loopback 形态不校验端口：端口被占用时插件会退到随机端口，粘贴回来的链接
// 带的是那个端口；host 仍只认回环地址，别家域名拼出来的照样拒绝。
func ParseCallbackCode(raw string) (code, state string, ok bool) {
	raw = strings.TrimSpace(raw)
	if index := strings.IndexAny(raw, "\r\n"); index >= 0 {
		raw = raw[:index]
	}
	parsed, errParse := url.Parse(raw)
	if errParse != nil {
		return "", "", false
	}
	deepLink := strings.EqualFold(parsed.Scheme, loginCallbackScheme) &&
		strings.EqualFold(parsed.Host, loginCallbackHost) &&
		parsed.Path == loginCallbackPath
	loopback := strings.EqualFold(parsed.Scheme, "http") &&
		isLoopbackHost(parsed.Hostname()) &&
		parsed.Path == CallbackPath
	if !deepLink && !loopback {
		return "", "", false
	}
	query := parsed.Query()
	code = strings.TrimSpace(query.Get("code"))
	if code == "" {
		code = strings.TrimSpace(query.Get("authorization_code"))
	}
	return code, query.Get("state"), code != ""
}

// isLoopbackHost 判断主机名是否回环地址。
//
// 同时认 localhost 与 127.0.0.1：授权页写死 127.0.0.1，但用户手工粘贴时
// 浏览器或代理可能把地址规范化成 localhost。
func isLoopbackHost(host string) bool {
	trimmed := strings.TrimSpace(host)
	return trimmed == "127.0.0.1" || strings.EqualFold(trimmed, "localhost")
}

// AuthorizeURLWith 构造官方授权页 URL。
//
// 参数名必须与官方桌面客户端逐字一致：login_source=desktop & appname=<中文
// 客户端名> & state=<state>。缺 appname 时授权页换票会拿到 200035
// （authorization_code_not_found），表现就是「授权完登录不成功」。
//
// desktop_callback=loopback + redirect 强制浏览器回跳本机：默认的
// office-raccoon:// 深链会被已安装的官方客户端截走，插件永远拿不到 code。
func AuthorizeURLWith(origin, appName, state, redirect string) string {
	query := url.Values{}
	query.Set("login_source", "desktop")
	query.Set("appname", appName)
	if strings.TrimSpace(state) != "" {
		query.Set("state", state)
	}
	query.Set("desktop_callback", "loopback")
	if strings.TrimSpace(redirect) != "" {
		query.Set("redirect", redirect)
	}
	return strings.TrimRight(origin, "/") + AuthorizePath + "?" + query.Encode()
}

// ---- loopback 回调监听 ----
//
// 一次登录期间在本机驻留一个一次性监听：授权页确认后浏览器直接打
// http://127.0.0.1:<port>/auth/callback，插件收到 code 当场换票，登录完成。
// 浏览器与插件不同机时回调打不到这里，地址栏仍留着完整链接，粘回控制台走
// SubmitCallback，两条路共用 completeLogin。

// CallbackURL 返回首选端口的本机回调地址（没有监听时供控制台展示）。
func CallbackURL() string { return callbackURL(CallbackPort) }

// callbackURL 拼出某个端口上的本机回调地址。
func callbackURL(port int) string {
	return fmt.Sprintf("http://%s:%d%s", callbackHost, port, CallbackPath)
}

// callbackCatcher 是一次登录期间驻留的回调监听。
type callbackCatcher struct {
	sessionID string
	state     string
	redirect  string
	server    *http.Server
	onCode    func(ctx context.Context, code string) (string, error)
	timer     *time.Timer
	stopOnce  sync.Once
}

var (
	catchersMu sync.Mutex
	// catchers 按会话登记在跑的监听：一次登录一个，登录结束即摘除。
	catchers = map[string]*callbackCatcher{}
)

// startCallbackCatcher 为会话启动回调监听，返回 redirect 与是否监听成功。
//
// 监听失败（端口全被占、无权限）不是错误：登录本身照常可走手工粘贴，
// 只是 redirect 指向一个没人接的端口，浏览器地址栏里仍留着完整链接。
func startCallbackCatcher(sessionID, state string, onCode func(ctx context.Context, code string) (string, error)) (string, bool) {
	listener, port, errListen := listenCallback()
	if errListen != nil {
		return CallbackURL(), false
	}
	catcher := &callbackCatcher{
		sessionID: sessionID,
		state:     state,
		redirect:  callbackURL(port),
		onCode:    onCode,
	}
	mux := http.NewServeMux()
	mux.HandleFunc(CallbackPath, catcher.handle)
	catcher.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	// 用 AfterFunc 而不是常驻 goroutine：TTL 到了由计时器直接收摊，没到期
	// 就随登录结束 Stop 掉，不留等待中的 goroutine。
	catcher.timer = time.AfterFunc(qrLoginTTL, catcher.stop)

	catchersMu.Lock()
	previous := catchers[sessionID]
	catchers[sessionID] = catcher
	catchersMu.Unlock()
	if previous != nil {
		previous.stop()
	}

	go func() {
		// Serve 在 Shutdown 后返回 ErrServerClosed，这里无需区分。
		_ = catcher.server.Serve(listener)
	}()
	return catcher.redirect, true
}

// listenCallback 绑定本机回调端口：先试固定端口，被占用时退到系统分配的端口。
func listenCallback() (net.Listener, int, error) {
	for _, address := range []string{
		fmt.Sprintf("%s:%d", callbackHost, CallbackPort),
		callbackHost + ":0",
	} {
		listener, errListen := net.Listen("tcp", address)
		if errListen != nil {
			continue
		}
		tcpAddr, okAddr := listener.Addr().(*net.TCPAddr)
		if !okAddr {
			_ = listener.Close()
			continue
		}
		return listener, tcpAddr.Port, nil
	}
	return nil, 0, fmt.Errorf("loopback callback port unavailable")
}

// stopCallbackCatcher 结束指定会话的监听（幂等）。
func stopCallbackCatcher(sessionID string) {
	catchersMu.Lock()
	catcher := catchers[sessionID]
	delete(catchers, sessionID)
	catchersMu.Unlock()
	if catcher != nil {
		catcher.stop()
	}
}

// callbackListening 报告该会话是否还有监听在跑，并返回它的回调地址。
func callbackListening(sessionID string) (string, bool) {
	catchersMu.Lock()
	defer catchersMu.Unlock()
	catcher := catchers[sessionID]
	if catcher == nil {
		return "", false
	}
	return catcher.redirect, true
}

// stop 关闭监听并释放端口（幂等）。
func (c *callbackCatcher) stop() {
	c.stopOnce.Do(func() {
		if c.timer != nil {
			c.timer.Stop()
		}
		catchersMu.Lock()
		if catchers[c.sessionID] == c {
			delete(catchers, c.sessionID)
		}
		catchersMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = c.server.Shutdown(ctx)
	})
}

// stopAsync 供回调 handler 收尾用。
//
// Shutdown 会等所有 handler 返回，在 handler 里同步调用会自锁，因此必须
// 另起 goroutine。
func (c *callbackCatcher) stopAsync() { go c.stop() }

// handle 处理授权页回跳。
func (c *callbackCatcher) handle(writer http.ResponseWriter, request *http.Request) {
	if !isLoopbackRemote(request.RemoteAddr) {
		writeCallbackPage(writer, http.StatusForbidden, "只接受本机回调")
		return
	}
	query := request.URL.Query()
	code := strings.TrimSpace(query.Get("code"))
	if code == "" {
		code = strings.TrimSpace(query.Get("authorization_code"))
	}
	if code == "" {
		writeCallbackPage(writer, http.StatusBadRequest, "回调缺少授权码，请回到控制台重新发起登录")
		return
	}
	// state 逐字比对：本机任何程序都能往这个端口丢请求，不校验就等于允许
	// 别人用自己的 code 覆盖当前这次登录。
	if strings.TrimSpace(query.Get("state")) != c.state {
		writeCallbackPage(writer, http.StatusBadRequest, "回调 state 校验失败，请重新发起登录")
		return
	}
	// 用独立超时而不是 request.Context()：用户可能在换票途中关掉页面，
	// 那不该把已经发出的换票请求一起取消。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	message, errComplete := c.onCode(ctx, code)
	if errComplete != nil {
		writeCallbackPage(writer, http.StatusBadRequest, "登录失败："+errComplete.Error())
		return
	}
	writeCallbackPage(writer, http.StatusOK, message)
	c.stopAsync()
}

// isLoopbackRemote 判断请求来源是否本机。
//
// 监听已经绑在 127.0.0.1 上，这里再按来源地址收一次，防止本机代理或
// 端口转发把外部请求带进来。
func isLoopbackRemote(remoteAddr string) bool {
	host, _, errSplit := net.SplitHostPort(strings.TrimSpace(remoteAddr))
	if errSplit != nil {
		return false
	}
	return isLoopbackHost(host)
}

// writeCallbackPage 渲染结果页。
//
// 只写固定文案与上游错误摘要，不回显任何请求参数：code/state 都是凭证材料，
// 落到页面上等于让截图与浏览历史把它们带走。错误摘要来自上游响应体，必须
// 转义后再拼进 HTML。
func writeCallbackPage(writer http.ResponseWriter, status int, message string) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_, _ = writer.Write([]byte("<!doctype html><html lang=\"zh-CN\"><head><meta charset=\"utf-8\">" +
		"<title>小浣熊登录</title><style>body{font-family:system-ui,-apple-system,'Segoe UI',sans-serif;" +
		"background:#0f1115;color:#e6e8ee;display:flex;align-items:center;justify-content:center;height:100vh;margin:0}" +
		"div{text-align:center;line-height:1.8}h1{font-size:18px;font-weight:600}p{color:#9aa3b2;font-size:14px}</style>" +
		"</head><body><div><h1>" + html.EscapeString(message) + "</h1><p>可以关闭此页面，回到控制台查看账号。</p></div></body></html>"))
}
