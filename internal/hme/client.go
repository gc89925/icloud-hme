// Package hme 实现了 iCloud Hide My Email 协议客户端。
//
// 基于 Cookie 会话,通过 tls-client 伪装 Chrome TLS 指纹规避 iCloud 风控。
// 对应原 Python 项目 icloud_hme.py 的 ICloudHME 类。
package hme

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

const (
	// ClientBuildNumber 是 iCloud Web 客户端构建号,从浏览器抓包获取。
	// maildomainws (HME 别名管理) 专用。
	ClientBuildNumber = "2624Build22"
	// ClientMasteringNumber 是 iCloud Web 客户端主版本号。
	ClientMasteringNumber = "2624Build22"
	// DefaultBuildNumber 用于 validate 和 mccgateway (邮件) 等非 HME 端点。
	DefaultBuildNumber = "2624Build13"
	// RequestTimeout 单次请求超时。
	RequestTimeout = 15 * time.Second
	// MaxRetries 最大尝试次数(仅对网络错误与 5xx 重试)。
	MaxRetries = 2
)

var retryDelays = []time.Duration{
	600 * time.Millisecond,
	1500 * time.Millisecond,
}

// AccountInfo 是从 /validate 响应中提取的账号身份信息。
type AccountInfo struct {
	DSID             string `json:"dsid"`
	AppleID          string `json:"appleId"`
	PrimaryEmail     string `json:"primaryEmail"`
	FullName         string `json:"fullName"`
	IsManagedAppleID bool   `json:"isManagedAppleId"`
}

// Alias 是一个 Hide My Email 隐私邮箱别名。
type Alias struct {
	Email       string `json:"email"`
	AnonymousID string `json:"anonymousId"`
	Label       string `json:"label"`
	Active      bool   `json:"active"`
	CreatedAt   string `json:"createdAt,omitempty"`
}

// Client 是 iCloud Hide My Email 客户端。
//
// 一个 Client 对应一个 iCloud 账号。通过传入的 Cookie 维持会话,
// 首次调用业务方法时会自动触发 ValidateSession 解析 HME 服务端点。
// Client 可被多个 goroutine 复用:Cookie 与会话状态由内部锁保护。
type Client struct {
	// Cookies 是当前会话 Cookie。并发场景请使用 CookiesSnapshot 读取。
	Cookies  map[string]string
	Host     string // "icloud.com" 或 "icloud.com.cn"
	Proxy    string // HTTP/SOCKS5 代理
	Username string // iCloud 账号 (用于登录)
	Password string // iCloud 密码 (用于登录)
	Verbose  bool

	mu          sync.RWMutex
	httpc       tls_client.HttpClient // 业务请求:不使用 cookie jar,Cookie 头完全由 Cookies 构造
	authc       tls_client.HttpClient // 登录流程:使用 cookie jar 串联 idmsa → setup
	setupURL    string
	serviceURL  string
	dsid        string // 从 validate 响应提取
	clientID    string // UUID,每次会话生成
	accountInfo *AccountInfo
	validatedAt time.Time
	cookieRev   uint64 // Cookie 每次变化自增,便于调用方判断是否需要持久化
}

// NewClient 创建一个新的 HME 客户端,底层使用 Chrome TLS 指纹。
//
// proxy 支持格式:
//   - HTTP:  "http://user:pass@host:port"
//   - SOCKS5: "socks5://user:pass@host:port"
func NewClient(cookies map[string]string, host, proxy string, verbose bool) (*Client, error) {
	if host == "" {
		host = "icloud.com"
	}
	base := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(int(RequestTimeout / time.Second)),
		tls_client.WithClientProfile(profiles.Chrome_146),
		tls_client.WithNotFollowRedirects(),
	}
	if proxy != "" {
		base = append(base, tls_client.WithProxyUrl(proxy))
	}
	httpc, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), base...)
	if err != nil {
		return nil, err
	}
	authOpts := append(append([]tls_client.HttpClientOption{}, base...),
		tls_client.WithCookieJar(tls_client.NewCookieJar()),
		tls_client.WithTimeoutSeconds(30),
	)
	authc, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), authOpts...)
	if err != nil {
		return nil, err
	}

	copied := make(map[string]string, len(cookies))
	for k, v := range cookies {
		copied[k] = v
	}
	return &Client{
		Cookies:  copied,
		Host:     normalizeHost(host),
		Proxy:    proxy,
		Verbose:  verbose,
		httpc:    httpc,
		authc:    authc,
		clientID: uuid.New().String(),
	}, nil
}

func normalizeHost(host string) string {
	h := strings.TrimSpace(strings.ToLower(host))
	if u, err := url.Parse(h); err == nil && u.Hostname() != "" {
		h = u.Hostname()
	} else if !strings.Contains(h, "://") {
		if u, err := url.Parse("https://" + h); err == nil && u.Hostname() != "" {
			h = u.Hostname()
		}
	}
	if strings.HasSuffix(h, ".icloud.com.cn") || h == "icloud.com.cn" {
		return "icloud.com.cn"
	}
	return "icloud.com"
}

// CookiesSnapshot 返回当前 Cookie 的副本。
func (c *Client) CookiesSnapshot() map[string]string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]string, len(c.Cookies))
	for k, v := range c.Cookies {
		out[k] = v
	}
	return out
}

// CookieRevision 返回 Cookie 版本号;每次 Cookie 被服务端刷新都会递增。
func (c *Client) CookieRevision() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cookieRev
}

// ValidatedAt 返回最近一次会话校验成功的时间。
func (c *Client) ValidatedAt() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.validatedAt
}

// SetupURL 返回 iCloud setup 端点。
func (c *Client) SetupURL() string {
	suffix := "setup.icloud.com"
	if c.Host == "icloud.com.cn" {
		suffix = "setup.icloud.com.cn"
	}
	return "https://" + suffix + "/setup/ws/1"
}

// Origin 返回 Web Origin。
func (c *Client) Origin() string {
	return "https://www." + c.Host
}

func (c *Client) log(format string, args ...any) {
	if c.Verbose {
		fmt.Printf("  [iCloud] %s\n", fmt.Sprintf(format, args...))
	}
}

// buildURL 给 URL 追加 clientBuildNumber / clientMasteringNumber / clientId / dsid 查询参数,
// 这是 iCloud Web API 的强制要求。
func (c *Client) buildURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	q := parsed.Query()
	host := parsed.Hostname()
	if strings.Contains(host, "maildomainws") {
		q.Set("clientBuildNumber", ClientBuildNumber)
		q.Set("clientMasteringNumber", ClientMasteringNumber)
	} else {
		q.Set("clientBuildNumber", DefaultBuildNumber)
		q.Set("clientMasteringNumber", DefaultBuildNumber)
	}
	if c.clientID != "" {
		q.Set("clientId", c.clientID)
	}
	c.mu.RLock()
	dsid := c.dsid
	c.mu.RUnlock()
	if dsid != "" {
		q.Set("dsid", dsid)
	}
	parsed.RawQuery = q.Encode()
	return parsed.String()
}

// requestOrigin 根据实际请求端点选择 Origin。
// Apple 可能把国区账号路由到全球服务，Origin 必须跟随目标域名而不是账号配置。
func requestOrigin(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err == nil {
		host := strings.ToLower(u.Hostname())
		if host == "icloud.com.cn" || strings.HasSuffix(host, ".icloud.com.cn") {
			return "https://www.icloud.com.cn"
		}
		if host == "icloud.com" || strings.HasSuffix(host, ".icloud.com") {
			return "https://www.icloud.com"
		}
	}
	return "https://www.icloud.com"
}

// cookieHeader 按浏览器行为原样拼接 Cookie(保留值中原有的双引号)。
func (c *Client) cookieHeader() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.Cookies) == 0 {
		return ""
	}
	keys := make([]string, 0, len(c.Cookies))
	for k := range c.Cookies {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+c.Cookies[k])
	}
	return strings.Join(parts, "; ")
}

// applySetCookies 解析原始 Set-Cookie 头并更新 Cookie。
//
// 只在成功响应上调用:Apple 在会话失效(421)时会下发清空所有 Cookie 的指令,
// 这种情况下保留原 Cookie,由上层把账号标记为失效,避免把账号数据写成空壳。
func (c *Client) applySetCookies(headers []string) {
	if len(headers) == 0 {
		return
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	changed := false
	for _, raw := range headers {
		name, value, expired := parseSetCookie(raw, now)
		if name == "" {
			continue
		}
		if expired || value == "" || value == `""` {
			if _, ok := c.Cookies[name]; ok {
				delete(c.Cookies, name)
				changed = true
			}
			continue
		}
		if c.Cookies[name] != value {
			c.Cookies[name] = value
			changed = true
		}
	}
	if changed {
		c.cookieRev++
	}
}

// parseSetCookie 解析一条 Set-Cookie,保留值的原始形式(含双引号)。
func parseSetCookie(raw string, now time.Time) (name, value string, expired bool) {
	parts := strings.Split(raw, ";")
	if len(parts) == 0 {
		return "", "", false
	}
	nv := strings.TrimSpace(parts[0])
	idx := strings.Index(nv, "=")
	if idx <= 0 {
		return "", "", false
	}
	name = strings.TrimSpace(nv[:idx])
	value = strings.TrimSpace(nv[idx+1:])
	for _, attr := range parts[1:] {
		attr = strings.TrimSpace(attr)
		k, v, _ := strings.Cut(attr, "=")
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "max-age":
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n <= 0 {
				expired = true
			}
		case "expires":
			for _, layout := range []string{time.RFC1123, "Mon, 02-Jan-2006 15:04:05 MST", time.RFC850} {
				if t, err := time.Parse(layout, strings.TrimSpace(v)); err == nil {
					if t.Before(now) {
						expired = true
					}
					break
				}
			}
		}
	}
	return name, value, expired
}

// request 执行 HTTP 请求,返回响应体字符串。
//
// 重试策略:仅对网络错误和 5xx 重试(最多 maxAttempts 次,短间隔);
// 401/403/421/450 表示会话失效,429 表示限流,都立即返回 *UpstreamError。
func (c *Client) request(method, rawURL string, body any, maxAttempts int) (string, error) {
	if maxAttempts <= 0 {
		maxAttempts = MaxRetries
	}
	fullURL := c.buildURL(rawURL)

	hostName := ""
	if u, err := url.Parse(rawURL); err == nil {
		hostName = u.Hostname()
	}
	contentType := "application/json"
	acceptType := "application/json, text/plain, */*"
	if strings.Contains(hostName, "maildomainws") {
		contentType = "text/plain"
		acceptType = "*/*"
	}

	var payload []byte
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return "", err
		}
		payload = buf
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		var reqBody io.Reader
		if payload != nil {
			reqBody = bytes.NewReader(payload)
		}
		req, err := http.NewRequest(method, fullURL, reqBody)
		if err != nil {
			return "", err
		}
		origin := requestOrigin(rawURL)
		req.Header = http.Header{
			"Accept":             {acceptType},
			"Accept-Language":    {"en-US,en;q=0.9,zh-CN;q=0.8,zh;q=0.7"},
			"Content-Type":       {contentType},
			"Origin":             {origin},
			"Referer":            {origin + "/"},
			"Sec-Fetch-Dest":     {"empty"},
			"Sec-Fetch-Mode":     {"cors"},
			"Sec-Fetch-Site":     {"same-site"},
			"sec-ch-ua":          {`"Google Chrome";v="147", "Not.A/Brand";v="8", "Chromium";v="147"`},
			"sec-ch-ua-mobile":   {"?0"},
			"sec-ch-ua-platform": {`"Windows"`},
			"User-Agent":         {"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36"},
		}
		if ck := c.cookieHeader(); ck != "" {
			req.Header.Set("Cookie", ck)
		}
		if c.Verbose {
			c.log(">>> %s %s", method, fullURL)
		}

		start := time.Now()
		resp, err := c.httpc.Do(req)
		if err != nil {
			lastErr = &UpstreamError{Kind: KindNetwork, Err: err}
			c.log("<<< 连接失败 (%s): %v", time.Since(start), err)
			if attempt < maxAttempts {
				sleepRetry(attempt)
				continue
			}
			return "", lastErr
		}
		text, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		c.log("<<< HTTP %d (%s)", resp.StatusCode, time.Since(start))

		status := resp.StatusCode
		if status >= 200 && status < 300 {
			c.applySetCookies(resp.Header.Values("Set-Cookie"))
			return string(text), nil
		}

		ue := &UpstreamError{Kind: KindUpstream, HTTPStatus: status}
		parsed := gjson.ParseBytes(text)
		ue.AppleCode = firstNonEmpty(parsed.Get("error.errorCode").String(), parsed.Get("errorCode").String())
		ue.AppleMsg = firstNonEmpty(parsed.Get("error.errorMessage").String(), parsed.Get("errorMessage").String(), parsed.Get("reason").String())
		switch {
		case status == 401 || status == 403 || status == 421 || status == 450:
			ue.Kind = KindSessionExpired
			return "", ue
		case status == 429:
			ue.Kind = KindRateLimited
			return "", ue
		}
		lastErr = ue
		if status >= 500 && attempt < maxAttempts {
			sleepRetry(attempt)
			continue
		}
		return "", lastErr
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", &UpstreamError{Kind: KindUpstream}
}

func sleepRetry(attempt int) {
	idx := attempt - 1
	if idx >= len(retryDelays) {
		idx = len(retryDelays) - 1
	}
	time.Sleep(retryDelays[idx])
}

// validationURLs 返回会话校验端点。
// 国区优先使用本地区域端点，并回退到全球端点以兼容 Apple 的路由调整。
func (c *Client) validationURLs() []string {
	primary := c.SetupURL() + "/validate"
	if c.Host != "icloud.com.cn" {
		return []string{primary}
	}
	global := "https://setup.icloud.com/setup/ws/1/validate"
	if primary == global {
		return []string{primary}
	}
	return []string{primary, global}
}

// ValidateSession 校验 iCloud 会话,解析 HME 服务端点和账号身份。
//
// 成功后会刷新 Cookie(服务端轮换 token),可作为保活调用。
// 失败时返回 *UpstreamError,其中 KindSessionExpired 表示 Cookie 已失效。
func (c *Client) ValidateSession() error {
	c.log("校验 iCloud 会话... (Cookie %d 个)", len(c.CookiesSnapshot()))
	var body string
	var err error
	urls := c.validationURLs()
	for i, validationURL := range urls {
		var candidate string
		candidate, err = c.request("POST", validationURL, nil, MaxRetries)
		if err == nil && !gjson.Valid(candidate) {
			err = &UpstreamError{Kind: KindUpstream, AppleMsg: "validate 响应不是有效 JSON"}
		}
		if err == nil && gjson.Get(candidate, "webservices.premiummailsettings.url").String() == "" {
			err = &UpstreamError{Kind: KindNotSubscribed}
		}
		if err == nil {
			body = candidate
			break
		}
		// 会话失效时换端点没有意义
		if IsSessionExpired(err) {
			break
		}
		if i < len(urls)-1 {
			c.log("区域 validate 失败，改用全球端点: %v", err)
		}
	}
	if err != nil {
		c.log("校验失败: %v", err)
		return withOp(err, "会话校验")
	}
	data := gjson.Parse(body)
	serviceURL := strings.TrimRight(data.Get("webservices.premiummailsettings.url").String(), "/")
	serviceURL = strings.TrimSuffix(serviceURL, ":443")

	dsInfo := data.Get("dsInfo")
	cookies := c.CookiesSnapshot()
	info := &AccountInfo{
		DSID:             dsInfo.Get("dsid").String(),
		AppleID:          firstNonEmpty(dsInfo.Get("appleId").String(), dsInfo.Get("primaryEmail").String(), dsInfo.Get("appleIdEmail").String()),
		PrimaryEmail:     firstNonEmpty(dsInfo.Get("primaryEmail").String(), dsInfo.Get("appleId").String()),
		FullName:         firstNonEmpty(dsInfo.Get("fullName").String(), dsInfo.Get("name").String()),
		IsManagedAppleID: dsInfo.Get("isManagedAppleId").Bool(),
	}
	if info.AppleID == "" {
		for _, name := range []string{"aosappleid", "appleId", "dsid"} {
			if v, ok := cookies[name]; ok && v != "" {
				info.AppleID = v
				break
			}
		}
	}
	c.mu.Lock()
	c.serviceURL = serviceURL
	c.dsid = info.DSID
	c.accountInfo = info
	c.validatedAt = time.Now()
	c.mu.Unlock()
	c.log("会话有效 → %s", nonEmpty(info.AppleID, "未知账号"))
	return nil
}

// AccountInfo 返回已校验的账号身份(校验前为 nil)。
func (c *Client) AccountInfo() *AccountInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.accountInfo
}

// resolveService 返回 HME 服务端点,必要时先校验会话。
func (c *Client) resolveService() (string, error) {
	c.mu.RLock()
	svc := c.serviceURL
	c.mu.RUnlock()
	if svc != "" {
		return svc, nil
	}
	if err := c.ValidateSession(); err != nil {
		return "", err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.serviceURL, nil
}

// resetService 清除缓存的服务端点,下次调用会重新校验会话。
func (c *Client) resetService() {
	c.mu.Lock()
	c.serviceURL = ""
	c.mu.Unlock()
}

// callService 调用 HME 服务;若服务端点已缓存但返回会话错误,会重新 validate 一次再试,
// 以区分「服务端点过期」和「会话真正失效」。
func (c *Client) callService(op, method, path string, body any, attempts int) (string, error) {
	svc, err := c.resolveService()
	if err != nil {
		return "", withOp(err, op)
	}
	resp, err := c.request(method, svc+path, body, attempts)
	if err != nil && IsSessionExpired(err) && !c.ValidatedAt().IsZero() && time.Since(c.ValidatedAt()) > 30*time.Second {
		c.resetService()
		if svc, verr := c.resolveService(); verr == nil {
			resp, err = c.request(method, svc+path, body, attempts)
		} else {
			err = verr
		}
	}
	if err != nil {
		return "", withOp(err, op)
	}
	return resp, nil
}

// businessError 把 {"success":false,"error":{...}} 转换为 *UpstreamError。
func businessError(op string, parsed gjson.Result) error {
	code := firstNonEmpty(parsed.Get("error.errorCode").String(), parsed.Get("errorCode").String())
	msg := firstNonEmpty(parsed.Get("error.errorMessage").String(), parsed.Get("errorMessage").String())
	ue := &UpstreamError{Kind: KindUpstream, Op: op, HTTPStatus: 200, AppleCode: code, AppleMsg: msg}
	if ra := parsed.Get("error.retryAfter"); ra.Exists() {
		ue.RetryAfter = int(ra.Int())
	}
	lower := strings.ToLower(msg)
	if code == "-41015" || strings.Contains(lower, "reached the limit") || strings.Contains(lower, "try again later") {
		ue.Kind = KindRateLimited
	}
	return ue
}

// ListAliases 列出当前账号所有 Hide My Email 别名。
func (c *Client) ListAliases() ([]Alias, error) {
	c.log("获取别名列表...")
	body, err := c.callService("获取别名列表", "GET", "/v2/hme/list", nil, MaxRetries)
	if err != nil {
		return nil, err
	}
	parsed := gjson.Parse(body)
	if parsed.Get("success").Exists() && !parsed.Get("success").Bool() {
		return nil, businessError("获取别名列表", parsed)
	}
	aliases := parseAliasList(body)
	c.log("共 %d 个别名", len(aliases))
	return aliases, nil
}

// Generate 生成一个候选别名(尚未保留,需再调用 Reserve)。
func (c *Client) Generate() (string, error) {
	c.log("生成候选别名...")
	body, err := c.callService("生成候选邮箱", "POST", "/v1/hme/generate", map[string]string{"langCode": "en-us"}, MaxRetries)
	if err != nil {
		return "", err
	}
	parsed := gjson.Parse(body)
	if !parsed.Get("success").Bool() {
		return "", businessError("生成候选邮箱", parsed)
	}
	hme := parsed.Get("result.hme").String()
	if hme == "" || strings.HasPrefix(hme, "{") {
		hme = firstNonEmpty(parsed.Get("result.hme.hme").String(), parsed.Get("result.hme.email").String())
	}
	if hme == "" {
		return "", &UpstreamError{Kind: KindUpstream, Op: "生成候选邮箱", AppleMsg: "响应中没有候选邮箱"}
	}
	c.log("候选: %s", hme)
	return hme, nil
}

// Reserve 保留/确认候选别名,使其正式生效。
func (c *Client) Reserve(hme, label string) (string, error) {
	if label == "" {
		label = "Created " + time.Now().Format("2006-01-02 15:04")
	}
	c.log("保留别名 %s ...", hme)
	payload := map[string]string{
		"hme":   hme,
		"label": label,
		"note":  "Created by icloud_hme tool",
	}
	// reserve 不是幂等操作,网络错误也不自动重试,避免重复创建
	body, err := c.callService("创建邮箱", "POST", "/v1/hme/reserve", payload, 1)
	if err != nil {
		return "", err
	}
	parsed := gjson.Parse(body)
	if !parsed.Get("success").Bool() {
		return "", businessError("创建邮箱", parsed)
	}
	alias := hme
	if v := parsed.Get("result.hme.hme").String(); v != "" {
		alias = v
	}
	c.log("已保留: %s", alias)
	return alias, nil
}

// CreateResult 是 CreateAlias 的返回结果。
type CreateResult struct {
	Email     string `json:"email"`
	Label     string `json:"label"`
	CreatedAt string `json:"created_at"`
}

// CreateAlias 一步完成「生成 + 保留」,创建一个新别名。
//
// 只对偶发的业务失败重试;会话失效、限流、网络错误立即返回,
// 避免像旧版本那样在被限流时连续重试 5 次、拖慢 20 秒还加重风控。
func (c *Client) CreateAlias(label string, maxRetries int) (*CreateResult, error) {
	if maxRetries <= 0 {
		maxRetries = 2
	}
	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			c.log("重试 %d/%d ...", attempt+1, maxRetries)
			time.Sleep(800 * time.Millisecond)
		}
		hme, err := c.Generate()
		if err == nil {
			var email string
			email, err = c.Reserve(hme, label)
			if err == nil {
				return &CreateResult{Email: email, Label: label, CreatedAt: time.Now().Format(time.RFC3339)}, nil
			}
		}
		lastErr = err
		if ue, ok := AsUpstreamError(err); ok && ue.Kind != KindUpstream {
			return nil, err
		}
	}
	return nil, lastErr
}

// DeactivateHME 停用别名(可恢复)。
func (c *Client) DeactivateHME(anonymousID string) (bool, error) {
	c.log("停用 %s ...", anonymousID)
	body, err := c.callService("停用别名", "POST", "/v1/hme/deactivate", map[string]string{"anonymousId": anonymousID}, MaxRetries)
	if err != nil {
		return false, err
	}
	parsed := gjson.Parse(body)
	if !parsed.Get("success").Bool() {
		return false, businessError("停用别名", parsed)
	}
	return true, nil
}

// ReactivateHME 激活已停用的别名。
func (c *Client) ReactivateHME(anonymousID string) (bool, error) {
	c.log("激活 %s ...", anonymousID)
	body, err := c.callService("激活别名", "POST", "/v1/hme/reactivate", map[string]string{"anonymousId": anonymousID}, MaxRetries)
	if err != nil {
		return false, err
	}
	parsed := gjson.Parse(body)
	if !parsed.Get("success").Bool() {
		return false, businessError("激活别名", parsed)
	}
	return true, nil
}

// Delete 删除别名。iCloud 只允许删除已停用的别名,直接删除失败时会先停用再删。
func (c *Client) Delete(anonymousID string) error {
	c.log("删除 %s ...", anonymousID)
	payload := map[string]string{"anonymousId": anonymousID}
	body, err := c.callService("删除别名", "POST", "/v1/hme/delete", payload, MaxRetries)
	if err == nil && gjson.Get(body, "success").Bool() {
		return nil
	}
	if err != nil {
		if ue, ok := AsUpstreamError(err); ok && ue.Kind != KindUpstream {
			return err
		}
	}
	c.log("直接删除失败,尝试先停用...")
	if _, derr := c.DeactivateHME(anonymousID); derr != nil {
		if ue, ok := AsUpstreamError(derr); ok && ue.Kind != KindUpstream {
			return derr
		}
	}
	body, err = c.callService("删除别名", "POST", "/v1/hme/delete", payload, MaxRetries)
	if err != nil {
		return err
	}
	parsed := gjson.Parse(body)
	if !parsed.Get("success").Bool() {
		return businessError("删除别名", parsed)
	}
	c.log("已删除")
	return nil
}

// ---- 别名列表解析 (对应 ICloudHME._parse_alias_list) ----

// parseAliasList 解析 iCloud 返回的别名列表 JSON。
// 容错:优先取 result.hmeEmails,找不到则递归查找第一个对象数组。
func parseAliasList(body string) []Alias {
	if !gjson.Valid(body) {
		return []Alias{}
	}
	root := gjson.Parse(body)

	arr := root.Get("result.hmeEmails")
	if !arr.IsArray() {
		arr = findFirstDictArray(root)
	}
	if !arr.IsArray() {
		return []Alias{}
	}

	aliases := []Alias{}
	arr.ForEach(func(_, item gjson.Result) bool {
		if !item.IsObject() {
			return true
		}
		meta := item.Get("metaData")
		email := strings.TrimSpace(strings.ToLower(firstNonEmpty(
			item.Get("hme").String(),
			item.Get("email").String(),
			item.Get("alias").String(),
			item.Get("address").String(),
			meta.Get("hme").String(),
		)))
		if email == "" || !strings.Contains(email, "@") {
			return true
		}
		state := strings.ToLower(firstNonEmpty(item.Get("state").String(), item.Get("status").String()))
		active := state != "inactive" && state != "deleted"
		if item.Get("active").Exists() {
			active = item.Get("active").Bool() && active
		}
		if item.Get("isActive").Exists() {
			active = item.Get("isActive").Bool() && active
		}
		aliases = append(aliases, Alias{
			Email:       email,
			AnonymousID: firstNonEmpty(item.Get("anonymousId").String(), item.Get("id").String()),
			Label:       firstNonEmpty(item.Get("label").String(), meta.Get("label").String()),
			Active:      active,
			CreatedAt:   firstNonEmpty(item.Get("createTimestamp").String(), item.Get("createdAt").String()),
		})
		return true
	})

	// 活跃的排前面,再按邮箱字母序。
	sort.SliceStable(aliases, func(i, j int) bool {
		if aliases[i].Active != aliases[j].Active {
			return aliases[i].Active
		}
		return aliases[i].Email < aliases[j].Email
	})
	return aliases
}

// findFirstDictArray 递归查找第一个「对象数组」。
func findFirstDictArray(v gjson.Result) gjson.Result {
	if v.IsArray() {
		if len(v.Array()) > 0 && v.Array()[0].IsObject() {
			return v
		}
	}
	if v.IsObject() {
		var found gjson.Result
		v.ForEach(func(_, val gjson.Result) bool {
			if r := findFirstDictArray(val); r.IsArray() && len(r.Array()) > 0 {
				found = r
				return false
			}
			return true
		})
		return found
	}
	return gjson.Result{}
}

// ---- 小工具 ----

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func nonEmpty(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
