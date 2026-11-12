package account

import (
	"log"
	"sync"
	"time"

	"icloud-hme/internal/hme"
	"icloud-hme/internal/mail"
)

// hmeEntry 是缓存的 HME 客户端:复用 TLS 连接与已解析的服务端点,
// 避免每个请求都重新 validate(旧版本每次操作都要 validate + 业务请求,约 3-6 秒)。
type hmeEntry struct {
	client     *hme.Client
	savedRev   uint64
	host       string
	proxy      string
	createdAt  time.Time
	lastUsedAt time.Time
}

type aliasEntry struct {
	aliases   []hme.Alias
	fetchedAt time.Time
}

type runtimeCache struct {
	mu      sync.Mutex
	clients map[string]*hmeEntry
	aliases map[string]aliasEntry
}

func newRuntimeCache() *runtimeCache {
	return &runtimeCache{clients: map[string]*hmeEntry{}, aliases: map[string]aliasEntry{}}
}

// invalidate 丢弃账号的客户端与别名缓存(Cookie/代理/主机变化、删除账号时调用)。
func (m *Manager) invalidate(id string) {
	m.cache.mu.Lock()
	delete(m.cache.clients, id)
	delete(m.cache.aliases, id)
	m.cache.mu.Unlock()
}

func (m *Manager) invalidateAll() {
	m.cache.mu.Lock()
	m.cache.clients = map[string]*hmeEntry{}
	m.cache.aliases = map[string]aliasEntry{}
	m.cache.mu.Unlock()
}

// HMEClient 返回指定账号的 HME 客户端(进程内复用)。
// 必须有有效的 Cookie 才能使用 HME 功能。使用后请调用 SyncClientCookies 持久化服务端刷新的 Cookie。
func (m *Manager) HMEClient(id string, verbose bool) (*hme.Client, error) {
	m.mu.RLock()
	acc, ok := m.accounts[id]
	var snap *Account
	if ok {
		snap = copyAccount(acc)
	}
	m.mu.RUnlock()
	if !ok {
		return nil, errAccountNotFound(id)
	}
	if len(snap.Cookies) == 0 {
		return nil, errNoCookies
	}

	m.cache.mu.Lock()
	defer m.cache.mu.Unlock()
	if e, ok := m.cache.clients[id]; ok && e.host == snap.Host && e.proxy == snap.Proxy {
		e.lastUsedAt = time.Now()
		return e.client, nil
	}
	client, err := hme.NewClient(snap.Cookies, snap.Host, snap.Proxy, verbose)
	if err != nil {
		return nil, err
	}
	m.cache.clients[id] = &hmeEntry{client: client, host: snap.Host, proxy: snap.Proxy, createdAt: time.Now(), lastUsedAt: time.Now()}
	return client, nil
}

// SyncClientCookies 在 Cookie 被 iCloud 刷新后写回 accounts.json(无变化时不写盘)。
func (m *Manager) SyncClientCookies(id string, client *hme.Client) {
	if client == nil {
		return
	}
	rev := client.CookieRevision()
	m.cache.mu.Lock()
	e, ok := m.cache.clients[id]
	if !ok || e.client != client || e.savedRev == rev {
		m.cache.mu.Unlock()
		return
	}
	e.savedRev = rev
	m.cache.mu.Unlock()

	cookies := client.CookiesSnapshot()
	if len(cookies) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if acc, ok := m.accounts[id]; ok {
		acc.Cookies = cookies
		_ = m.save()
	}
}

// CachedAliases 返回未过期的别名缓存。
func (m *Manager) CachedAliases(id string, maxAge time.Duration) ([]hme.Alias, time.Time, bool) {
	m.cache.mu.Lock()
	defer m.cache.mu.Unlock()
	e, ok := m.cache.aliases[id]
	if !ok || time.Since(e.fetchedAt) > maxAge {
		return nil, time.Time{}, false
	}
	out := make([]hme.Alias, len(e.aliases))
	copy(out, e.aliases)
	return out, e.fetchedAt, true
}

// StoreAliases 缓存别名列表,并同步账号的别名统计与状态。
func (m *Manager) StoreAliases(id string, aliases []hme.Alias) time.Time {
	now := time.Now()
	cp := make([]hme.Alias, len(aliases))
	copy(cp, aliases)
	m.cache.mu.Lock()
	m.cache.aliases[id] = aliasEntry{aliases: cp, fetchedAt: now}
	m.cache.mu.Unlock()

	active := 0
	for _, a := range aliases {
		if a.Active {
			active++
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if acc, ok := m.accounts[id]; ok {
		changed := acc.AliasTotal != len(aliases) || acc.AliasActive != active || acc.Status != "active" || acc.LastError != "" || acc.StatusDetail != ""
		acc.AliasTotal = len(aliases)
		acc.AliasActive = active
		acc.Status = "active"
		acc.LastError = ""
		acc.StatusDetail = ""
		acc.LastValidated = now.Format(time.RFC3339)
		if changed {
			_ = m.save()
		}
	}
	return now
}

// InvalidateAliases 让别名缓存失效(创建/删除/停用/激活后调用)。
func (m *Manager) InvalidateAliases(id string) {
	m.cache.mu.Lock()
	delete(m.cache.aliases, id)
	m.cache.mu.Unlock()
}

// MarkUpstreamResult 根据 iCloud 调用结果更新账号状态:
// 会话失效 → status=error 并记录原因;成功 → 清除错误。其它错误不改变状态。
func (m *Manager) MarkUpstreamResult(id string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	acc, ok := m.accounts[id]
	if !ok {
		return
	}
	switch {
	case err == nil:
		if acc.Status != "active" || acc.LastError != "" {
			acc.Status = "active"
			acc.LastError = ""
			acc.StatusDetail = ""
			acc.LastValidated = time.Now().Format(time.RFC3339)
			_ = m.save()
		}
	case hme.IsSessionExpired(err):
		msg := truncate(err.Error(), 300)
		if acc.Status != "error" || acc.LastError != msg {
			acc.Status = "error"
			acc.LastError = msg
			acc.StatusDetail = msg
			_ = m.save()
		}
	}
}

// StartKeepalive 定期校验所有有效账号的 iCloud 会话,并持久化服务端轮换后的 Cookie。
//
// iCloud Web 会话长时间无请求会被服务端注销(validate 返回 421 并清空 Cookie),
// 保活可以让面板长期可用而无需频繁重新粘贴 Cookie。interval <= 0 时不启动。
func (m *Manager) StartKeepalive(interval time.Duration) (stop func()) {
	if interval <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		// 启动后稍等再做第一次,避免和启动期请求挤在一起
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-done:
				return
			case <-timer.C:
				m.keepaliveOnce()
				timer.Reset(interval)
			}
		}
	}()
	return func() { close(done) }
}

func (m *Manager) keepaliveOnce() {
	m.mu.RLock()
	ids := make([]string, 0, len(m.accounts))
	for id, acc := range m.accounts {
		if len(acc.Cookies) > 0 && acc.Status != "error" {
			ids = append(ids, id)
		}
	}
	m.mu.RUnlock()
	for _, id := range ids {
		client, err := m.HMEClient(id, false)
		if err != nil {
			continue
		}
		err = client.ValidateSession()
		m.SyncClientCookies(id, client)
		m.MarkUpstreamResult(id, err)
		if err != nil {
			log.Printf("iCloud 会话保活失败 account=%s: %v", id, err)
		}
	}
}

// mailCreds 返回账号的 IMAP 登录信息:外部邮箱优先,其次 iCloud App 专用密码。
func (m *Manager) mailCreds(id string) (mail.Credentials, error) {
	m.mu.RLock()
	acc, ok := m.accounts[id]
	var snap *Account
	if ok {
		snap = copyAccount(acc)
		if acc.Mailbox != nil {
			mb := *acc.Mailbox
			snap.Mailbox = &mb
		}
	}
	m.mu.RUnlock()
	if !ok {
		return mail.Credentials{}, errAccountNotFound(id)
	}
	if snap.Mailbox != nil && snap.Mailbox.Email != "" && snap.Mailbox.Password != "" {
		return mail.Credentials{Username: snap.Mailbox.Email, Password: snap.Mailbox.Password, Server: snap.Mailbox.IMAPHost, Port: snap.Mailbox.IMAPPort}, nil
	}
	imapEmail := firstNonEmpty(snap.ICloudEmail, snap.RealEmail)
	if !isICloudDomain(imapEmail) {
		return mail.Credentials{}, errNoMailbox
	}
	if snap.AppPassword == "" {
		return mail.Credentials{}, errNoMailbox
	}
	return mail.Credentials{Username: imapEmail, Password: snap.AppPassword, Server: mail.IMAPServer, Port: mail.IMAPPort}, nil
}

// HasMailbox 判断账号是否配置了 IMAP 收信(外部邮箱或 App 专用密码)。
func (m *Manager) HasMailbox(id string) bool {
	_, err := m.mailCreds(id)
	return err == nil
}
