// Package server - 可替换业务接口与 Manager 适配器。
//
// Backend 边界固定为高层业务动作,不把具体 *hme.Client 或 *mail.Client 暴露给 handler。
package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/mail"
)

// BackendError 是后端返回的稳定错误,携带 HTTP 状态码与稳定错误码。
type BackendError struct {
	Status  int
	Code    string
	Message string
}

func (e *BackendError) Error() string { return e.Message }

// InboxQuery 是收件箱查询参数。
type InboxQuery struct {
	AccountID string
	Alias     string
	Limit     int
	Days      int
}

// InboxResult 是收件箱查询结果。
type InboxResult struct {
	AccountID string         `json:"account_id"`
	Alias     string         `json:"alias,omitempty"`
	Count     int            `json:"count"`
	Messages  []mail.Message `json:"messages"`
	Method    string         `json:"method"`
}

// Backend 是可替换的业务接口;handler 只依赖本接口,测试使用内存 fake。
type Backend interface {
	ListAccounts() []account.Summary
	AddAccount(account.AddAccountInput) (account.Summary, error)
	UpdateAccount(string, account.UpdateAccountInput) (account.Summary, error)
	UpdateProxy(string, string) (account.Summary, error)
	UpdateCookies(string, string) (account.Summary, error)
	SetAppPassword(string, string, string) (account.Summary, error)
	SetMailbox(string, account.MailboxConfig) (account.Summary, error)
	LoginAccount(string, string, string) (account.Summary, error)
	RemoveAccount(string) bool
	CreateAlias(string, string) (*hme.CreateResult, error)
	ListAliases(string, bool) (AliasList, error)
	SetAliasActive(string, string, bool) (bool, error)
	DeleteAlias(string, string) error
	UpdateAliasLabel(string, string, string, *string) (hme.Alias, error)
	ListInbox(InboxQuery) (InboxResult, error)
	GetMessage(string, uint32) (*mail.FullMessage, error)
	DeleteMessage(string, uint32) error
	Reload() error
}

// managerBackend 是生产 Backend,包装 *account.Manager。
type managerBackend struct {
	mgr *account.Manager
}

// ListAccounts 返回账号安全摘要列表。
func (b *managerBackend) ListAccounts() []account.Summary {
	return b.mgr.ListSummaries()
}

// AddAccount 添加账号。
func (b *managerBackend) AddAccount(in account.AddAccountInput) (account.Summary, error) {
	sum, err := b.mgr.AddAccountWithInput(in)
	if err != nil {
		return account.Summary{}, &BackendError{Status: http.StatusBadRequest, Code: "VALIDATION_ERROR", Message: err.Error()}
	}
	return sum, nil
}

// UpdateAccount 编辑账号基本信息。
func (b *managerBackend) UpdateAccount(id string, in account.UpdateAccountInput) (account.Summary, error) {
	sum, err := b.mgr.UpdateMetadata(id, in)
	if err != nil {
		return account.Summary{}, mapAccountErr(err)
	}
	return sum, nil
}

// UpdateProxy 更新或清除账号代理。
func (b *managerBackend) UpdateProxy(id, proxy string) (account.Summary, error) {
	sum, err := b.mgr.UpdateProxy(id, proxy)
	if err != nil {
		return account.Summary{}, mapAccountErr(err)
	}
	return sum, nil
}

// UpdateCookies 更新账号 Cookie。cookies 为原始文本(Header String 或 JSON)。
func (b *managerBackend) UpdateCookies(id, cookies string) (account.Summary, error) {
	parsed, err := account.ParseCookieInput(cookies)
	if err != nil {
		return account.Summary{}, &BackendError{Status: http.StatusBadRequest, Code: "VALIDATION_ERROR", Message: err.Error()}
	}
	if err := b.mgr.UpdateCookies(id, parsed); err != nil {
		return account.Summary{}, mapAccountErr(err)
	}
	sum, ok := b.mgr.GetAccount(id)
	if !ok {
		return account.Summary{}, &BackendError{Status: http.StatusNotFound, Code: "ACCOUNT_NOT_FOUND", Message: "账号不存在"}
	}
	return sum.Summary(), nil
}

// SetAppPassword 设置 iCloud 邮箱与 App 专用密码并测试 IMAP 连接。
func (b *managerBackend) SetAppPassword(id, icloudEmail, appPassword string) (account.Summary, error) {
	if err := b.mgr.SetAppPassword(id, icloudEmail, appPassword); err != nil {
		msg := err.Error()
		if strings.Contains(msg, "账号不存在") {
			return account.Summary{}, &BackendError{Status: http.StatusNotFound, Code: "ACCOUNT_NOT_FOUND", Message: "账号不存在"}
		}
		if strings.Contains(msg, "不能为空") {
			return account.Summary{}, &BackendError{Status: http.StatusBadRequest, Code: "VALIDATION_ERROR", Message: msg}
		}
		// IMAP 连接失败属于上游错误,不拼接详细错误
		return account.Summary{}, &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "IMAP 验证失败,请检查邮箱与 App 专用密码"}
	}
	sum, ok := b.mgr.GetAccount(id)
	if !ok {
		return account.Summary{}, &BackendError{Status: http.StatusNotFound, Code: "ACCOUNT_NOT_FOUND", Message: "账号不存在"}
	}
	return sum.Summary(), nil
}

// SetMailbox configures and verifies an external IMAP mailbox.
func (b *managerBackend) SetMailbox(id string, config account.MailboxConfig) (account.Summary, error) {
	if err := b.mgr.SetMailbox(id, config); err != nil {
		if strings.Contains(err.Error(), "账号不存在") {
			return account.Summary{}, &BackendError{Status: http.StatusNotFound, Code: "ACCOUNT_NOT_FOUND", Message: "账号不存在"}
		}
		return account.Summary{}, &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "收件邮箱验证失败,请检查邮箱、授权码和 IMAP 配置"}
	}
	sum, ok := b.mgr.GetAccount(id)
	if !ok {
		return account.Summary{}, &BackendError{Status: http.StatusNotFound, Code: "ACCOUNT_NOT_FOUND", Message: "账号不存在"}
	}
	return sum.Summary(), nil
}

// LoginAccount 使用 iCloud 密码登录账号,成功只返回 Summary,绝不返回 Cookies。
func (b *managerBackend) LoginAccount(id, password, otpCode string) (account.Summary, error) {
	var otpProvider hme.OTPProvider
	if otpCode != "" {
		otp := otpCode
		otpProvider = func() (string, error) { return otp, nil }
	}

	client, err := b.mgr.HMEClientWithPassword(id, password, otpProvider)
	if err != nil {
		return account.Summary{}, classifyLoginErr(err)
	}
	_ = client
	sum, ok := b.mgr.GetAccount(id)
	if !ok {
		return account.Summary{}, &BackendError{Status: http.StatusNotFound, Code: "ACCOUNT_NOT_FOUND", Message: "账号不存在"}
	}
	return sum.Summary(), nil
}

// classifyLoginErr 把 iCloud 登录错误映射为稳定错误。
func classifyLoginErr(err error) *BackendError {
	msg := err.Error()
	if strings.Contains(msg, "需要提供 OTP") {
		return &BackendError{Status: http.StatusConflict, Code: "OTP_REQUIRED", Message: "需要提供 OTP 验证码"}
	}
	if strings.Contains(msg, "2FA 验证失败") {
		return &BackendError{Status: http.StatusUnauthorized, Code: "OTP_INVALID", Message: "OTP 验证码错误"}
	}
	if strings.Contains(msg, "账号不存在") {
		return &BackendError{Status: http.StatusNotFound, Code: "ACCOUNT_NOT_FOUND", Message: "账号不存在"}
	}
	if isSessionError(msg) {
		return &BackendError{Status: http.StatusUnauthorized, Code: "UPSTREAM_UNAUTHORIZED", Message: "iCloud 会话失效,请更新 Cookie"}
	}
	return &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "iCloud 登录失败,请稍后重试"}
}

// RemoveAccount 删除账号。
func (b *managerBackend) RemoveAccount(id string) bool {
	return b.mgr.RemoveAccount(id)
}

// aliasCacheTTL 别名列表缓存时间。别名变更操作会主动让缓存失效,所以可以适当长一些。
const aliasCacheTTL = 2 * time.Minute

// AliasList 是别名列表结果。
type AliasList struct {
	Aliases   []hme.Alias
	FetchedAt time.Time
	Cached    bool
}

// withHME 获取账号的 HME 客户端执行 fn,并统一处理 Cookie 持久化与账号状态。
func (b *managerBackend) withHME(accountID, op string, fn func(*hme.Client) error) error {
	client, err := b.mgr.HMEClient(accountID, false)
	if err != nil {
		return mapAccountErr(err)
	}
	err = fn(client)
	b.mgr.SyncClientCookies(accountID, client)
	b.mgr.MarkUpstreamResult(accountID, err)
	if err != nil {
		return classifyUpstreamErr(op, err)
	}
	return nil
}

// CreateAlias 创建 HME 别名。
func (b *managerBackend) CreateAlias(accountID, label string) (*hme.CreateResult, error) {
	var result *hme.CreateResult
	err := b.withHME(accountID, "创建邮箱失败", func(c *hme.Client) error {
		var e error
		result, e = c.CreateAlias(label, 2)
		return e
	})
	if err != nil {
		return nil, err
	}
	b.mgr.InvalidateAliases(accountID)
	return result, nil
}

// ListAliases 列出账号的 HME 别名;refresh=false 时优先使用短期缓存。
func (b *managerBackend) ListAliases(accountID string, refresh bool) (AliasList, error) {
	if !refresh {
		if aliases, at, ok := b.mgr.CachedAliases(accountID, aliasCacheTTL); ok {
			return AliasList{Aliases: aliases, FetchedAt: at, Cached: true}, nil
		}
	}
	var aliases []hme.Alias
	err := b.withHME(accountID, "获取别名列表失败", func(c *hme.Client) error {
		var e error
		aliases, e = c.ListAliases()
		return e
	})
	if err != nil {
		return AliasList{}, err
	}
	at := b.mgr.StoreAliases(accountID, aliases)
	return AliasList{Aliases: aliases, FetchedAt: at}, nil
}

// SetAliasActive 停用或激活别名。
func (b *managerBackend) SetAliasActive(accountID, anonymousID string, active bool) (bool, error) {
	op := "停用失败"
	if active {
		op = "激活失败"
	}
	var success bool
	err := b.withHME(accountID, op, func(c *hme.Client) error {
		var e error
		if active {
			success, e = c.ReactivateHME(anonymousID)
		} else {
			success, e = c.DeactivateHME(anonymousID)
		}
		return e
	})
	b.mgr.InvalidateAliases(accountID)
	if err != nil {
		return false, err
	}
	return success, nil
}

// DeleteAlias 删除别名。
func (b *managerBackend) DeleteAlias(accountID, anonymousID string) error {
	err := b.withHME(accountID, "删除失败", func(c *hme.Client) error {
		return c.Delete(anonymousID)
	})
	b.mgr.InvalidateAliases(accountID)
	return err
}

// UpdateAliasLabel 修改别名标签。note 为 nil 时沿用 iCloud 上的原备注(避免被接口覆盖清空)。
func (b *managerBackend) UpdateAliasLabel(accountID, anonymousID, label string, note *string) (hme.Alias, error) {
	var current *hme.Alias
	if note == nil {
		list, err := b.ListAliases(accountID, false)
		if err != nil {
			return hme.Alias{}, err
		}
		for i := range list.Aliases {
			if list.Aliases[i].AnonymousID == anonymousID {
				current = &list.Aliases[i]
				break
			}
		}
		if current == nil {
			// 缓存可能过期,强制刷新再找一次
			if list, err = b.ListAliases(accountID, true); err != nil {
				return hme.Alias{}, err
			}
			for i := range list.Aliases {
				if list.Aliases[i].AnonymousID == anonymousID {
					current = &list.Aliases[i]
					break
				}
			}
		}
		if current == nil {
			return hme.Alias{}, &BackendError{Status: http.StatusNotFound, Code: "ALIAS_NOT_FOUND", Message: "别名不存在或已被删除"}
		}
		n := current.Note
		note = &n
	}
	err := b.withHME(accountID, "修改标签失败", func(c *hme.Client) error {
		return c.UpdateLabel(anonymousID, label, *note)
	})
	b.mgr.InvalidateAliases(accountID)
	if err != nil {
		return hme.Alias{}, err
	}
	out := hme.Alias{AnonymousID: anonymousID, Label: label, Note: *note}
	if current != nil {
		out.Email, out.Active, out.CreatedAt = current.Email, current.Active, current.CreatedAt
	}
	return out, nil
}
// ListInbox 读取收件箱摘要:IMAP(外部邮箱或 App 专用密码)优先,Web API(Cookie)回退。
func (b *managerBackend) ListInbox(q InboxQuery) (InboxResult, error) {
	var imapErr error
	if b.mgr.HasMailbox(q.AccountID) {
		var imapMessages []mail.Message
		imapErr = b.mgr.WithMailClient(q.AccountID, func(mc *mail.Client) error {
			var e error
			if q.Alias != "" {
				imapMessages, e = mc.FindByRecipient(q.Alias, q.Limit, q.Days)
			} else {
				imapMessages, e = mc.ListInbox(q.Limit, q.Days)
			}
			return e
		})
		if imapErr == nil {
			if imapMessages == nil {
				imapMessages = []mail.Message{}
			}
			return InboxResult{AccountID: q.AccountID, Alias: q.Alias, Count: len(imapMessages), Messages: imapMessages, Method: "imap"}, nil
		}
	}

	// 回退到 Web API (Cookie 认证,无需 App Password)
	wmc, err := b.mgr.WebMailClient(q.AccountID)
	if err != nil {
		if imapErr != nil {
			return InboxResult{}, &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "读取邮件失败: " + mailErrMessage(imapErr)}
		}
		return InboxResult{}, &BackendError{Status: http.StatusBadRequest, Code: "VALIDATION_ERROR", Message: "无可用邮件客户端: 请配置收件邮箱授权码 / App 专用密码,或 iCloud Cookie"}
	}
	var messages []mail.Message
	if q.Alias != "" {
		messages, err = wmc.FindByAlias(q.Alias, q.Limit)
	} else {
		messages, err = wmc.ListInbox(q.Limit)
	}
	if err != nil {
		msg := "读取邮件失败: Web 接口返回 " + truncateMsg(err.Error(), 120)
		if imapErr != nil {
			msg = "读取邮件失败: IMAP " + mailErrMessage(imapErr) + ";Web 接口也失败"
		}
		return InboxResult{}, &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: msg}
	}
	return InboxResult{AccountID: q.AccountID, Alias: q.Alias, Count: len(messages), Messages: messages, Method: "web_api"}, nil
}

func (b *managerBackend) GetMessage(accountID string, uid uint32) (*mail.FullMessage, error) {
	var message *mail.FullMessage
	err := b.mgr.WithMailClient(accountID, func(mc *mail.Client) error {
		var e error
		message, e = mc.GetFull(uid)
		return e
	})
	if err != nil {
		if errors.Is(err, account.ErrNoMailbox) || strings.Contains(err.Error(), "账号不存在") {
			return nil, mapAccountErr(err)
		}
		return nil, &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "读取邮件详情失败: " + mailErrMessage(err)}
	}
	return message, nil
}

func (b *managerBackend) DeleteMessage(accountID string, uid uint32) error {
	err := b.mgr.WithMailClient(accountID, func(mc *mail.Client) error {
		return mc.Delete(uid)
	})
	if err != nil {
		if errors.Is(err, account.ErrNoMailbox) || strings.Contains(err.Error(), "账号不存在") {
			return mapAccountErr(err)
		}
		return &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "删除邮件失败: " + mailErrMessage(err)}
	}
	return nil
}

// mailErrMessage 把 IMAP 错误转换为简短中文原因。
func mailErrMessage(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	lower := strings.ToLower(s)
	switch {
	case strings.Contains(s, "登录失败") || strings.Contains(lower, "authenticat") || strings.Contains(lower, "login fail"):
		return "邮箱登录失败,请检查授权码/App 专用密码是否已失效"
	case strings.Contains(lower, "timeout") || strings.Contains(lower, "i/o timeout"):
		return "连接邮箱服务器超时"
	case strings.Contains(s, "连接失败") || strings.Contains(lower, "connection refused") || strings.Contains(lower, "no such host"):
		return "无法连接邮箱服务器"
	case strings.Contains(s, "邮件不存在"):
		return "邮件不存在或已被删除"
	}
	return truncateMsg(s, 120)
}

func truncateMsg(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
// Reload 重新加载配置。
func (b *managerBackend) Reload() error {
	if err := b.mgr.Reload(); err != nil {
		return &BackendError{Status: http.StatusInternalServerError, Code: "INTERNAL_ERROR", Message: "重新加载配置失败"}
	}
	return nil
}

// mapAccountErr 把账号管理器错误映射为稳定错误。
func mapAccountErr(err error) *BackendError {
	msg := err.Error()
	if strings.Contains(msg, "账号不存在") {
		return &BackendError{Status: http.StatusNotFound, Code: "ACCOUNT_NOT_FOUND", Message: "账号不存在"}
	}
	if strings.Contains(msg, "Cookie") && strings.Contains(msg, "未配置") {
		return &BackendError{Status: http.StatusBadRequest, Code: "VALIDATION_ERROR", Message: "账号未配置 Cookie"}
	}
	return &BackendError{Status: http.StatusBadRequest, Code: "VALIDATION_ERROR", Message: msg}
}

// classifyUpstreamErr 把上游 (iCloud) 错误映射为稳定错误码,并附带可读的失败原因
// (HTTP 状态、Apple 错误码等,不包含 Cookie 等敏感信息)。
func classifyUpstreamErr(fixedMsg string, err error) *BackendError {
	if err == nil {
		return nil
	}
	if ue, ok := hme.AsUpstreamError(err); ok {
		switch ue.Kind {
		case hme.KindSessionExpired:
			return &BackendError{Status: http.StatusUnauthorized, Code: "UPSTREAM_UNAUTHORIZED", Message: ue.Message()}
		case hme.KindRateLimited:
			return &BackendError{Status: http.StatusTooManyRequests, Code: "UPSTREAM_RATE_LIMITED", Message: ue.Message()}
		case hme.KindNetwork:
			return &BackendError{Status: http.StatusGatewayTimeout, Code: "UPSTREAM_UNREACHABLE", Message: ue.Message()}
		case hme.KindNotSubscribed:
			return &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_NOT_SUBSCRIBED", Message: ue.Message()}
		default:
			return &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: ue.Message()}
		}
	}
	if isSessionError(err.Error()) {
		return &BackendError{Status: http.StatusUnauthorized, Code: "UPSTREAM_UNAUTHORIZED", Message: "iCloud 会话失效,请更新 Cookie"}
	}
	return &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: fixedMsg + ": " + truncateMsg(err.Error(), 160)}
}

// isSessionError 判断(非结构化)错误是否由会话失效引起,用于登录流程等旧路径。
func isSessionError(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "http 401") || strings.Contains(m, "http 403") || strings.Contains(m, "http 421") ||
		strings.Contains(m, "unauthorized") || strings.Contains(m, "会话校验失败")
}
// asBackendError 提取 BackendError,非 BackendError 统一为 INTERNAL_ERROR。
func asBackendError(err error) *BackendError {
	var be *BackendError
	if errors.As(err, &be) {
		return be
	}
	return &BackendError{Status: http.StatusInternalServerError, Code: "INTERNAL_ERROR", Message: "内部错误"}
}

// cookieInputToJSON 把 handler 解析出的 map 转回 JSON 文本,交给 ParseCookieInput。
func cookieInputToJSON(cookies map[string]string) string {
	raw, err := json.Marshal(cookies)
	if err != nil {
		return ""
	}
	return string(raw)
}
