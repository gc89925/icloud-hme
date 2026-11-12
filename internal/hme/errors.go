package hme

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrorKind 是上游错误的稳定分类,供 server 层映射 HTTP 状态与提示文案。
type ErrorKind string

const (
	// KindSessionExpired iCloud 会话失效(HTTP 401/403/421/450),需要重新提供 Cookie 或登录。
	KindSessionExpired ErrorKind = "session_expired"
	// KindRateLimited Apple 限流(HTTP 429 或业务错误码 -41015 等)。
	KindRateLimited ErrorKind = "rate_limited"
	// KindNetwork 连接/超时/代理错误。
	KindNetwork ErrorKind = "network"
	// KindNotSubscribed 账号没有 Hide My Email 服务(未订阅 iCloud+ 或服务未开通)。
	KindNotSubscribed ErrorKind = "not_subscribed"
	// KindUpstream 其它上游错误(5xx、业务失败等)。
	KindUpstream ErrorKind = "upstream"
)

// UpstreamError 描述一次 iCloud 调用失败的原因,Message 为可直接展示给用户的中文说明。
type UpstreamError struct {
	Kind       ErrorKind
	Op         string // 操作描述,例如 "会话校验"、"获取别名列表"
	HTTPStatus int    // 上游 HTTP 状态码(0 表示没有收到响应)
	AppleCode  string // Apple 业务错误码,例如 -41015
	AppleMsg   string // Apple 业务错误信息
	RetryAfter int    // 建议重试等待秒数(Apple 返回时)
	Err        error  // 底层错误(网络错误等)
}

func (e *UpstreamError) Error() string {
	return e.Message()
}

func (e *UpstreamError) Unwrap() error { return e.Err }

// Message 返回面向用户的失败原因。
func (e *UpstreamError) Message() string {
	op := e.Op
	if op == "" {
		op = "iCloud 请求"
	}
	switch e.Kind {
	case KindSessionExpired:
		return fmt.Sprintf("%s失败: iCloud 会话已失效(Apple 返回 HTTP %d,已清除登录态)。请到「账号」页重新粘贴 Cookie,或使用密码登录", op, e.HTTPStatus)
	case KindRateLimited:
		detail := "Apple 限制了短时间内的请求次数"
		if e.AppleCode == "-41015" {
			detail = "Apple 限制: 短时间内创建的隐藏邮箱过多(错误码 -41015)"
		} else if e.AppleCode != "" {
			detail += fmt.Sprintf("(错误码 %s)", e.AppleCode)
		}
		return fmt.Sprintf("%s失败: %s,请稍后再试(通常需等待 30-60 分钟)", op, detail)
	case KindNetwork:
		return fmt.Sprintf("%s失败: 无法连接 iCloud(%s)。请检查 NAS 网络/代理是否能访问 icloud.com", op, redact(errString(e.Err)))
	case KindNotSubscribed:
		return fmt.Sprintf("%s失败: 该账号没有可用的「隐藏邮件地址」服务(需要 iCloud+ 订阅)", op)
	}
	var parts []string
	if e.HTTPStatus > 0 {
		parts = append(parts, fmt.Sprintf("HTTP %d", e.HTTPStatus))
	}
	if e.AppleCode != "" {
		parts = append(parts, "错误码 "+e.AppleCode)
	}
	if e.AppleMsg != "" {
		parts = append(parts, truncateRunes(e.AppleMsg, 160))
	}
	if len(parts) == 0 && e.Err != nil {
		parts = append(parts, redact(errString(e.Err)))
	}
	if len(parts) == 0 {
		return op + "失败: iCloud 返回未知错误"
	}
	return fmt.Sprintf("%s失败: iCloud 返回 %s", op, strings.Join(parts, ", "))
}

// AsUpstreamError 提取 *UpstreamError。
func AsUpstreamError(err error) (*UpstreamError, bool) {
	var ue *UpstreamError
	if errors.As(err, &ue) {
		return ue, true
	}
	return nil, false
}

// IsSessionExpired 判断错误是否代表 iCloud 会话失效。
func IsSessionExpired(err error) bool {
	ue, ok := AsUpstreamError(err)
	return ok && ue.Kind == KindSessionExpired
}

func withOp(err error, op string) error {
	if ue, ok := AsUpstreamError(err); ok {
		cp := *ue
		cp.Op = op
		return &cp
	}
	return err
}

func errString(err error) string {
	if err == nil {
		return "未知错误"
	}
	return err.Error()
}

var credentialInURL = regexp.MustCompile(`(://)[^/@\s:]+(:[^/@\s]*)?@`)

// redact 去掉错误信息中可能包含的代理账号密码。
func redact(s string) string {
	s = credentialInURL.ReplaceAllString(s, "$1***@")
	return truncateRunes(s, 200)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
