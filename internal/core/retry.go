package core

// 本文件是「上游调用重试」的唯一实现：凭证过期保活一次、传输层故障等待后
// 重试一次。控制台额度面板与宿主额度面板都必须走这里。
//
// 之所以要下沉到 core：宿主面板（app.handleQuotaFetch）与控制台
// （admin.queryQuota）查询的是同一批上游、同一批凭证，但此前只有控制台
// 做了保活与重试，宿主面板直接单发一次——MiniMax 的 access token 只有
// 1 小时有效期，于是「控制台刷新成功、宿主面板 401」这种分裂会周期性出现。

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// QuotaRetryDelay 是瞬时故障重试前的等待。
const QuotaRetryDelay = 500 * time.Millisecond

// QueryWithRetry 执行一次上游查询并处理两种重试。
//
//   - 凭证过期（401）：先保活刷新一次，用新凭证重查。MiniMax 的 access token
//     只有 1 小时有效期，不刷新的话额度列会一直挂着 401。
//   - 传输层故障（transient）：上游 EOF / 连接重置在实测里成片出现，等待后
//     重查一次；重试必须沿用刷新后的凭证，否则会把新 token 换回旧的再打一次 401。
//
// 拆成泛型函数是为了可测：真实依赖（保活、账号池）由调用方注入。
func QueryWithRetry[T any](
	ctx context.Context,
	query func(*Credential) (T, error),
	credential *Credential,
	refresh func() *Credential,
) (T, error) {
	result, used, errQuery := WithAuthRetry(credential, refresh, query)
	if errQuery != nil && IsTransientFailure(errQuery) && SleepContext(ctx, QuotaRetryDelay) {
		result, errQuery = query(used)
	}
	return result, errQuery
}

// WithAuthRetry 在动作因凭证过期（401）失败时刷新一次凭证再重试。
//
// 返回最终结果、实际使用的凭证（重试时是刷新后的）与错误。刷新失败时保留原
// 错误、不重试：用旧凭证再打一次只会得到同一个 401。
func WithAuthRetry[T any](
	credential *Credential,
	refresh func() *Credential,
	action func(*Credential) (T, error),
) (T, *Credential, error) {
	result, errAction := action(credential)
	if errAction == nil || !IsAuthExpired(errAction) || refresh == nil {
		return result, credential, errAction
	}
	refreshed := refresh()
	if refreshed == nil {
		return result, credential, errAction
	}
	result, errAction = action(refreshed)
	return result, refreshed, errAction
}

// IsTransientFailure 判断错误是否属于「稍后重试可能成功」的传输层故障。
//
// 只认分类结果：按状态码猜（502/503 一律重试）会把「上游明确拒绝」也算进去。
func IsTransientFailure(err error) bool {
	var failure *Failure
	return errors.As(err, &failure) && failure.Kind == FailureKindTransient
}

// TaskRetryAttempts 是任务类上游调用的总尝试次数（含首次）。
const TaskRetryAttempts = 3

// TaskRetryBaseDelay 是指数退避的基数：0.8s、1.6s。
const TaskRetryBaseDelay = 800 * time.Millisecond

// IsRetryableFailure 判断任务类调用是否值得重试。
//
// 与 IsTransientFailure 的区别在 429：额度查询把 429 当「限流」直接展示给
// 用户，重试只会把面板拖慢；签到与任务族里 429 是「过几秒就好」的软限流，
// 一次就判失败会让账号白丢当天的签到。
func IsRetryableFailure(err error) bool {
	var failure *Failure
	if !errors.As(err, &failure) {
		return false
	}
	switch failure.Kind {
	case FailureKindTransient, FailureKindSoftRate, FailureKindRateLimit, FailureKindBusy, FailureKindServer:
		return true
	}
	return false
}

// WithTaskRetry 在可重试失败上做指数退避，上游给了 Retry-After 就按它等。
//
// fn 必须幂等：签到、活跃上报、旅行状态查询在上游都是「做过就返回已完成」，
// 重放一次不会重复发放。动作类接口（领取、下单）不要套这一层。
func WithTaskRetry(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 0; attempt < TaskRetryAttempts; attempt++ {
		if attempt > 0 {
			delay := TaskRetryBaseDelay << (attempt - 1)
			if hinted := RetryAfterOf(err); hinted > 0 {
				delay = hinted
			}
			if !SleepContext(ctx, delay) {
				return ctx.Err()
			}
		}
		if err = fn(); err == nil {
			return nil
		}
		if !IsRetryableFailure(err) {
			return err
		}
	}
	return err
}

// IsAuthExpired 判断错误是否属于「凭证已过期/失效」类（401）。
//
// 只认 401：403/404 往往意味着账号没这个权限或上游改版，刷新不会变好，
// 盲目刷新只会白白多打一次上游。
func IsAuthExpired(err error) bool {
	var failure *Failure
	if errors.As(err, &failure) {
		// 只看归一后的状态码：403 的凭证类错误（无权限）刷新不会变好。
		return failure.HTTPStatus() == http.StatusUnauthorized
	}
	var pluginErr *PluginError
	if errors.As(err, &pluginErr) {
		return pluginErr.HTTPStatus == http.StatusUnauthorized
	}
	return false
}

// IsDefinitiveFailure 判断错误是否表示「账号侧已有确定结论」。
//
// 判定用途是「这次没取到额度，还能不能拿上次的读数顶上」：
//   - 瞬时类（限流、超时、5xx、繁忙、WAF 拦截）过一会儿再查就能恢复，
//     展示层可以保留上次读数并明确标注非实时；
//   - 确定类（硬额度耗尽、凭证失效、内容拦截、参数错误、账号异常）不会
//     自己好转，必须如实报错——否则界面会把「额度已经没了」显示成上次的
//     正常读数，用户照着旧数字去用，撞墙才发现。
//
// 非 Failure（网络层错误、上下文取消）一律按瞬时处理：调用方无从判断账号
// 状态，保留上次读数并标注非实时是可接受的。
func IsDefinitiveFailure(err error) bool {
	if err == nil {
		return false
	}
	// 凭证侧的状态码同样是确定结论：401/403 换时间重试不会自己好转，凭证
	// 得重新授权才能用。漏掉这一条会让页面把「凭证已失效」显示成上次的正常
	// 读数（见 #71 第二轮复审 P2-4）。
	var pluginErr *PluginError
	if errors.As(err, &pluginErr) && pluginErr != nil {
		if pluginErr.HTTPStatus == 401 || pluginErr.HTTPStatus == 403 {
			return true
		}
	}
	var failure *Failure
	if !errors.As(err, &failure) {
		return false
	}
	switch failure.Kind {
	case FailureKindHardCredit, FailureKindQuota,
		FailureKindSessionDead, FailureKindCredential,
		FailureKindContentBlocked, FailureKindChannelRejected,
		FailureKindBadParams, FailureKindClient,
		FailureKindAccountFault, FailureKindModelBlocked,
		FailureKindNotFound, FailureKindPromptTooLong, FailureKindImageInvalid:
		return true
	}
	return false
}
