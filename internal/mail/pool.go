// IMAP 连接池: 按「服务器+账号」复用长连接, 避免每次读信/删信都重新 TLS 握手 + 登录。
package mail

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Pool 管理按账号复用的 IMAP 长连接。同一账号串行使用(go-imap 非并发安全)。
type Pool struct {
	mu    sync.Mutex
	items map[string]*pooledConn
	// idleClose 空闲超过该时间则下次使用前重建; 0 表示不主动关。
	idleClose time.Duration
	// pingAfter 距上次使用超过该时间才在借出前发 NOOP 探活。
	pingAfter time.Duration
}

// Credentials 描述一个 IMAP 登录身份。
type Credentials struct {
	Username string
	Password string
	Server   string
	Port     int
}

func (c Credentials) key() string {
	server := c.Server
	if server == "" {
		server = IMAPServer
	}
	port := c.Port
	if port == 0 {
		port = IMAPPort
	}
	return fmt.Sprintf("%s:%d|%s", strings.ToLower(server), port, strings.ToLower(c.Username))
}

type pooledConn struct {
	mu       sync.Mutex
	creds    Credentials
	client   *Client
	lastUsed time.Time
}

// NewPool 创建连接池。
func NewPool() *Pool {
	return &Pool{
		items:     make(map[string]*pooledConn),
		idleClose: 10 * time.Minute,
		pingAfter: 45 * time.Second,
	}
}

// Do 借出 iCloud IMAP 连接执行 fn(兼容旧接口)。
func (p *Pool) Do(appleID, appPassword string, fn func(*Client) error) error {
	return p.DoWith(Credentials{Username: appleID, Password: appPassword}, fn)
}

// DoWith 借出已连接的 Client 执行 fn; 用完不 Logout, 连接留在池中。
// 复用的旧连接若在执行中断开,会自动重连并重试一次。
func (p *Pool) DoWith(creds Credentials, fn func(*Client) error) error {
	if creds.Username == "" || creds.Password == "" {
		return fmt.Errorf("IMAP 凭据为空")
	}
	pc := p.getOrCreate(creds)
	pc.mu.Lock()
	defer pc.mu.Unlock()

	for attempt := 0; attempt < 2; attempt++ {
		reused, err := pc.ensure(p.idleClose, p.pingAfter)
		if err != nil {
			return err
		}
		err = fn(pc.client)
		pc.lastUsed = time.Now()
		if err != nil && isLikelyConnErr(err) {
			// 连接坏了, 丢掉; 若是复用的旧连接则重连后重试一次
			pc.client.forceClose()
			pc.client = nil
			if reused && attempt == 0 {
				continue
			}
		}
		return err
	}
	return nil
}

// Invalidate 关闭并移除指定账号的连接(凭据变更时调用)。
func (p *Pool) Invalidate(creds Credentials) {
	p.mu.Lock()
	pc, ok := p.items[creds.key()]
	if ok {
		delete(p.items, creds.key())
	}
	p.mu.Unlock()
	if ok {
		pc.mu.Lock()
		if pc.client != nil {
			pc.client.forceClose()
			pc.client = nil
		}
		pc.mu.Unlock()
	}
}

// Close 关闭池内全部连接。
func (p *Pool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, pc := range p.items {
		pc.mu.Lock()
		if pc.client != nil {
			pc.client.Disconnect()
			pc.client = nil
		}
		pc.mu.Unlock()
		delete(p.items, k)
	}
}

func (p *Pool) getOrCreate(creds Credentials) *pooledConn {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := creds.key()
	if pc, ok := p.items[key]; ok {
		if pc.creds.Password != creds.Password {
			// 密码变更:等当前使用者结束后再替换
			pc.mu.Lock()
			if pc.client != nil {
				pc.client.forceClose()
				pc.client = nil
			}
			pc.creds = creds
			pc.mu.Unlock()
		}
		return pc
	}
	pc := &pooledConn{creds: creds}
	p.items[key] = pc
	return pc
}

// ensure 保证 pc.client 可用;返回值 reused 表示是否复用了已有连接。
func (pc *pooledConn) ensure(idleClose, pingAfter time.Duration) (reused bool, err error) {
	if pc.client != nil && idleClose > 0 && !pc.lastUsed.IsZero() && time.Since(pc.lastUsed) > idleClose {
		// 空闲太久主动重建, 避免服务端静默断连
		pc.client.forceClose()
		pc.client = nil
	}
	if pc.client != nil {
		if time.Since(pc.lastUsed) < pingAfter {
			return true, nil
		}
		if err := pc.client.Ping(); err == nil {
			return true, nil
		}
		pc.client.forceClose()
		pc.client = nil
	}
	c := NewClientWithServer(pc.creds.Username, pc.creds.Password, pc.creds.Server, pc.creds.Port)
	if err := c.Connect(); err != nil {
		return false, err
	}
	pc.client = c
	pc.lastUsed = time.Now()
	return false, nil
}

func isLikelyConnErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, k := range []string{
		"connection reset", "broken pipe", "eof", "i/o timeout", "timeout",
		"use of closed", "not connected", "connection refused", "connection closed",
		"imap 连接", "wsarecv", "wsasend", "未连接",
	} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}
