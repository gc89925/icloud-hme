// Package mail 实现邮件 IMAP 读取客户端。
//
// 支持 iCloud(App 专用密码,imap.mail.me.com:993)以及 QQ/Gmail 等外部邮箱,
// 用于读取隐私邮箱别名转发过来的邮件。
package mail

import (
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
)

const (
	IMAPServer = "imap.mail.me.com"
	IMAPPort   = 993

	// listFetchBytes 列表页每封邮件最多拉取的字节数(只为生成摘要,不拉附件)。
	listFetchBytes = 64 << 10
	// dialTimeout 建立 IMAP 连接的超时。
	dialTimeout = 15 * time.Second
	// commandTimeout 单个 IMAP 命令的超时,防止上游卡死拖住整个请求。
	commandTimeout = 45 * time.Second
)

// Message 是一封邮件的摘要信息。
type Message struct {
	ID      string `json:"id"`
	From    string `json:"from"`
	To      string `json:"to"`
	Subject string `json:"subject"`
	Date    string `json:"date"`
	Preview string `json:"preview"`
	Size    uint32 `json:"size,omitempty"`

	sortTime time.Time
}

// FullMessage 是一封邮件的完整内容(含正文)。
type FullMessage struct {
	Message
	Body        string `json:"body"`
	ContentType string `json:"content_type"`
}

// Client 是 IMAP 客户端(非并发安全,需由 Pool 串行使用)。
type Client struct {
	username string
	password string
	server   string
	port     int
	cli      *client.Client
}

// NewClient 创建 iCloud IMAP 客户端。需在调用其它方法前先 Connect。
func NewClient(appleID, appPassword string) *Client {
	return NewClientWithServer(appleID, appPassword, IMAPServer, IMAPPort)
}

// NewClientWithServer creates an IMAP client for a custom server.
func NewClientWithServer(username, password, server string, port int) *Client {
	if server == "" {
		server = IMAPServer
	}
	if port == 0 {
		port = IMAPPort
	}
	return &Client{username: username, password: password, server: server, port: port}
}

// Connect 连接并登录 IMAP 服务器。已连接且存活时直接复用。
func (c *Client) Connect() error {
	if c.cli != nil {
		if err := c.cli.Noop(); err == nil {
			return nil
		}
		c.forceClose()
	}
	addr := fmt.Sprintf("%s:%d", c.server, c.port)
	cli, err := client.DialWithDialerTLS(&net.Dialer{Timeout: dialTimeout}, addr, nil)
	if err != nil {
		return fmt.Errorf("IMAP 连接失败(%s): %w", addr, err)
	}
	cli.Timeout = commandTimeout
	if err := cli.Login(c.username, c.password); err != nil {
		_ = cli.Logout()
		return fmt.Errorf("IMAP 登录失败 — 请检查邮箱账号、授权码和服务器地址: %w", err)
	}
	c.cli = cli
	return nil
}

// Ping 探测连接是否仍可用(NOOP)。
func (c *Client) Ping() error {
	if c.cli == nil {
		return fmt.Errorf("未连接")
	}
	return c.cli.Noop()
}

// Disconnect 登出并关闭连接。
func (c *Client) Disconnect() {
	if c.cli != nil {
		_ = c.cli.Logout()
		c.cli = nil
	}
}

// forceClose 不发 LOGOUT, 直接掐断(坏连接/池丢弃时用)。
func (c *Client) forceClose() {
	if c.cli != nil {
		_ = c.cli.Terminate()
		c.cli = nil
	}
}

// InboxCount 返回收件箱邮件总数。
func (c *Client) InboxCount() (int, error) {
	if c.cli == nil {
		return 0, fmt.Errorf("未连接")
	}
	mbox, err := c.cli.Select("INBOX", true)
	if err != nil {
		return 0, err
	}
	return int(mbox.Messages), nil
}

// ListInbox 拉取收件箱最近 limit 封邮件摘要(新→旧)。
//
// days > 0 时用服务端 SEARCH SINCE 过滤;每封邮件只拉取前 64KB 用于生成摘要,
// 全部邮件在一次 UID FETCH 中批量取回。
func (c *Client) ListInbox(limit int, days int) ([]Message, error) {
	if c.cli == nil {
		return nil, fmt.Errorf("未连接")
	}
	if limit <= 0 {
		limit = 50
	}
	mbox, err := c.cli.Select("INBOX", true)
	if err != nil {
		return nil, err
	}
	if mbox.Messages == 0 {
		return []Message{}, nil
	}

	if days > 0 {
		criteria := imap.NewSearchCriteria()
		criteria.Since = time.Now().AddDate(0, 0, -days)
		if uids, serr := c.cli.UidSearch(criteria); serr == nil {
			return c.fetchSummaries(newestUIDs(sortUIDs(uids), limit), days)
		}
		// 个别服务器不支持 SEARCH 时退回序号范围 + 本地过滤
	}

	from := uint32(1)
	if uint32(limit) < mbox.Messages {
		from = mbox.Messages - uint32(limit) + 1
	}
	seqset := new(imap.SeqSet)
	seqset.AddRange(from, mbox.Messages)
	msgs, err := c.fetch(seqset, false)
	if err != nil {
		return nil, err
	}
	return finalize(msgs, days, limit), nil
}

// FindByRecipient 查找发给指定隐私邮箱别名的最近 limit 封邮件(新→旧)。
func (c *Client) FindByRecipient(recipient string, limit int, days int) ([]Message, error) {
	var out []Message
	err := c.ForEachByRecipient(recipient, limit, days, func(m Message) bool {
		out = append(out, m)
		return true
	})
	if out == nil {
		out = []Message{}
	}
	return out, err
}

// ForEachByRecipient 按新→旧遍历发给 recipient 的最近 limit 封邮件。
// onMsg 返回 false 时立即停止。
func (c *Client) ForEachByRecipient(recipient string, limit int, days int, onMsg func(Message) bool) error {
	if c.cli == nil {
		return fmt.Errorf("未连接")
	}
	if onMsg == nil {
		return fmt.Errorf("onMsg 不能为空")
	}
	if limit <= 0 {
		limit = 5
	}
	if _, err := c.cli.Select("INBOX", true); err != nil {
		return err
	}

	// 1) 服务端按 To 搜索
	criteria := imap.NewSearchCriteria()
	criteria.Header.Add("To", recipient)
	if days > 0 {
		criteria.Since = time.Now().AddDate(0, 0, -days)
	}
	uids, err := c.cli.UidSearch(criteria)
	var msgs []Message
	if err == nil {
		// 服务端搜索成功(包括 0 结果)直接使用,不再额外扫描最近邮件
		msgs, err = c.fetchSummaries(newestUIDs(sortUIDs(uids), limit), days)
		if err != nil {
			return err
		}
	} else {
		// 2) fallback: 服务器不支持按 To 搜索时,扫最近若干封信封, 本地匹配后再批量取正文
		msgs, err = c.recentMatching(recipient, limit, days)
		if err != nil {
			return err
		}
	}
	for _, m := range msgs {
		if !onMsg(m) {
			return nil
		}
	}
	return nil
}

// recentMatching 拉取收件箱最近 scan 封(仅信封), 本地按 To 过滤后批量取摘要。
func (c *Client) recentMatching(recipient string, limit int, days int) ([]Message, error) {
	mbox := c.cli.Mailbox()
	if mbox == nil || mbox.Messages == 0 {
		return []Message{}, nil
	}
	scan := uint32(limit * 4)
	if scan < 40 {
		scan = 40
	}
	if scan > 200 {
		scan = 200
	}
	if scan > mbox.Messages {
		scan = mbox.Messages
	}
	seqset := new(imap.SeqSet)
	seqset.AddRange(mbox.Messages-scan+1, mbox.Messages)

	ch := make(chan *imap.Message, 16)
	done := make(chan error, 1)
	go func() { done <- c.cli.Fetch(seqset, []imap.FetchItem{imap.FetchUid, imap.FetchEnvelope}, ch) }()

	recipient = strings.ToLower(recipient)
	var uids []uint32
	for msg := range ch {
		if msg == nil || msg.Envelope == nil {
			continue
		}
		matched := false
		for _, list := range [][]*imap.Address{msg.Envelope.To, msg.Envelope.Cc} {
			for _, a := range list {
				if strings.EqualFold(a.Address(), recipient) {
					matched = true
				}
			}
		}
		if !matched {
			continue
		}
		if days > 0 && !msg.Envelope.Date.IsZero() && time.Since(msg.Envelope.Date) > time.Duration(days)*24*time.Hour {
			continue
		}
		uids = append(uids, msg.Uid)
	}
	if err := <-done; err != nil {
		return nil, err
	}
	return c.fetchSummaries(newestUIDs(sortUIDs(uids), limit), days)
}

// fetchSummaries 一次性批量拉取指定 UID 的摘要。
func (c *Client) fetchSummaries(uids []uint32, days int) ([]Message, error) {
	if len(uids) == 0 {
		return []Message{}, nil
	}
	seqset := new(imap.SeqSet)
	seqset.AddNum(uids...)
	msgs, err := c.fetch(seqset, true)
	if err != nil {
		return nil, err
	}
	return finalize(msgs, days, len(uids)), nil
}

// fetch 执行一次批量 FETCH:信封 + 大小 + 正文前 64KB(BODY.PEEK,不标记已读)。
func (c *Client) fetch(seqset *imap.SeqSet, byUID bool) ([]Message, error) {
	section := &imap.BodySectionName{Peek: true, Partial: []int{0, listFetchBytes}}
	items := []imap.FetchItem{imap.FetchUid, imap.FetchEnvelope, imap.FetchInternalDate, imap.FetchRFC822Size, section.FetchItem()}
	ch := make(chan *imap.Message, 16)
	done := make(chan error, 1)
	go func() {
		if byUID {
			done <- c.cli.UidFetch(seqset, items, ch)
		} else {
			done <- c.cli.Fetch(seqset, items, ch)
		}
	}()
	var out []Message
	for msg := range ch {
		if msg == nil {
			continue
		}
		m := toMessage(msg)
		if raw := firstBody(msg); raw != nil {
			data, _ := io.ReadAll(raw)
			m.Preview = makePreview(parseRawMessage(data).Text)
		}
		out = append(out, m)
	}
	if err := <-done; err != nil {
		return nil, err
	}
	return out, nil
}

// finalize 按天数过滤、按时间倒序排序并截断到 limit。
func finalize(msgs []Message, days, limit int) []Message {
	out := make([]Message, 0, len(msgs))
	cutoff := time.Time{}
	if days > 0 {
		cutoff = time.Now().AddDate(0, 0, -days)
	}
	for _, m := range msgs {
		if !cutoff.IsZero() && !m.sortTime.IsZero() && m.sortTime.Before(cutoff) {
			continue
		}
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].sortTime.After(out[j].sortTime) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func sortUIDs(uids []uint32) []uint32 {
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
	return uids
}

// newestUIDs 保留 UID 列表中最新的 limit 个(假定 UID 升序)。
func newestUIDs(uids []uint32, limit int) []uint32 {
	if limit <= 0 || len(uids) <= limit {
		return uids
	}
	return uids[len(uids)-limit:]
}

// GetFull 获取单封邮件的完整内容(含正文),使用 BODY.PEEK 不标记已读。
func (c *Client) GetFull(uid uint32) (*FullMessage, error) {
	if c.cli == nil {
		return nil, fmt.Errorf("未连接")
	}
	if _, err := c.cli.Select("INBOX", true); err != nil {
		return nil, err
	}
	seqset := new(imap.SeqSet)
	seqset.AddNum(uid)
	section := &imap.BodySectionName{Peek: true}
	items := []imap.FetchItem{imap.FetchUid, imap.FetchEnvelope, imap.FetchInternalDate, imap.FetchRFC822Size, section.FetchItem()}
	ch := make(chan *imap.Message, 1)
	done := make(chan error, 1)
	go func() { done <- c.cli.UidFetch(seqset, items, ch) }()

	var full *FullMessage
	for msg := range ch {
		if msg == nil || full != nil {
			continue
		}
		full = &FullMessage{Message: toMessage(msg)}
		if raw := firstBody(msg); raw != nil {
			data, _ := io.ReadAll(raw)
			parsed := parseRawMessage(data)
			full.Body = parsed.Text
			full.ContentType = parsed.ContentType
			full.Preview = makePreview(parsed.Text)
		}
	}
	if err := <-done; err != nil {
		return nil, err
	}
	if full == nil {
		return nil, fmt.Errorf("邮件不存在 (uid=%d)", uid)
	}
	return full, nil
}

// Delete 删除收件箱中指定 UID 的邮件。
//
// 服务器支持 UIDPLUS 时只 expunge 这一封(UID EXPUNGE),否则退回普通 EXPUNGE。
func (c *Client) Delete(uid uint32) error {
	if c.cli == nil {
		return fmt.Errorf("未连接")
	}
	if uid == 0 {
		return fmt.Errorf("邮件 UID 无效")
	}
	if _, err := c.cli.Select("INBOX", false); err != nil {
		return err
	}
	seqset := new(imap.SeqSet)
	seqset.AddNum(uid)
	item := imap.FormatFlagsOp(imap.AddFlags, true)
	if err := c.cli.UidStore(seqset, item, []interface{}{imap.DeletedFlag}, nil); err != nil {
		return err
	}
	if ok, _ := c.cli.Support("UIDPLUS"); ok {
		if _, err := c.cli.Execute(&uidExpungeCmd{seqset: seqset}, nil); err == nil {
			return nil
		}
	}
	return c.cli.Expunge(nil)
}

// uidExpungeCmd 实现 RFC 4315 UID EXPUNGE。
type uidExpungeCmd struct{ seqset *imap.SeqSet }

func (cmd *uidExpungeCmd) Command() *imap.Command {
	return &imap.Command{Name: "UID EXPUNGE", Arguments: []interface{}{cmd.seqset}}
}

// ---- 解析工具 ----

func firstBody(msg *imap.Message) imap.Literal {
	for _, lit := range msg.Body {
		if lit != nil {
			return lit
		}
	}
	return nil
}

func toMessage(msg *imap.Message) Message {
	m := Message{Size: msg.Size}
	if msg.Uid > 0 {
		m.ID = fmt.Sprintf("%d", msg.Uid)
	}
	if msg.Envelope != nil {
		if len(msg.Envelope.From) > 0 {
			m.From = decodeHeader(formatAddress(msg.Envelope.From[0]))
		}
		if len(msg.Envelope.To) > 0 {
			addrs := make([]string, 0, len(msg.Envelope.To))
			for _, a := range msg.Envelope.To {
				addrs = append(addrs, a.Address())
			}
			m.To = decodeHeader(strings.Join(addrs, ", "))
		}
		m.Subject = decodeHeader(msg.Envelope.Subject)
		if !msg.Envelope.Date.IsZero() {
			m.sortTime = msg.Envelope.Date
		}
	}
	if m.sortTime.IsZero() || (!msg.InternalDate.IsZero() && m.sortTime.After(msg.InternalDate.Add(24*time.Hour))) {
		// 信封日期缺失或明显伪造(未来时间)时以服务器接收时间为准
		m.sortTime = msg.InternalDate
	}
	if !m.sortTime.IsZero() {
		m.Date = m.sortTime.Format(time.RFC3339)
	}
	return m
}

// formatAddress 只返回邮箱地址;显示名在列表中意义不大且常含编码垃圾。
func formatAddress(a *imap.Address) string {
	if a == nil {
		return ""
	}
	return a.Address()
}
