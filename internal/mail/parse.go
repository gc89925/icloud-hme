package mail

import (
	"bytes"
	"io"
	"mime"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset" // 注册 GBK/GB18030/Big5 等字符集解码
)

const (
	// PreviewMaxRunes 列表摘要最大字符数。
	PreviewMaxRunes = 160
	// BodyMaxBytes 详情正文最大字节数,防止超大邮件拖慢接口。
	BodyMaxBytes = 512 << 10
	// partMaxBytes 单个 MIME 文本段最多读取的字节数。
	partMaxBytes = 2 << 20
)

var (
	urlRE        = regexp.MustCompile(`(?i)[<(\[]?\b(?:https?://|www\.)[^\s<>"']+[>)\]]?`)
	emptyBrRE    = regexp.MustCompile(`<\s*>|\(\s*\)|\[\s*\]`)
	spaceRunRE   = regexp.MustCompile(`[ \t\x{00a0}\x{200b}\x{200c}\x{200d}\x{feff}]+`)
	invisibleRE  = regexp.MustCompile(`[\x{034f}\x{200b}-\x{200f}\x{2028}-\x{202f}\x{2060}-\x{206f}\x{feff}\x{00ad}]`)
	blankLinesRE = regexp.MustCompile(`\n{3,}`)
)

// parsedBody 是从原始邮件(RFC 822)中提取出的正文。
type parsedBody struct {
	Text        string // 可读纯文本(HTML 已转换)
	ContentType string // 顶层 Content-Type
}

// parseRawMessage 解析原始邮件,正确处理 multipart、base64/quoted-printable 与各种字符集。
//
// raw 可以是被截断的邮件(列表页只拉取前 64KB),解析时会尽量保留已读到的文本。
func parseRawMessage(raw []byte) parsedBody {
	out := parsedBody{}
	// 字符集未知时 go-message 仍会返回 entity(正文保持原始字节),这里只在完全无法解析时退化
	entity, _ := message.Read(bytes.NewReader(raw))
	if entity == nil {
		return parsedBody{Text: normalizeBody(sanitizePlainPreview(string(raw)))}
	}
	out.ContentType = entity.Header.Get("Content-Type")

	var plain, html strings.Builder
	walkEntity(entity, &plain, &html, 0)

	plainText := strings.TrimSpace(plain.String())
	htmlText := ""
	if html.Len() > 0 {
		htmlText = strings.TrimSpace(stripHTML(html.String()))
	}
	plainRunes := utf8.RuneCountInString(plainText)
	// 有些邮件的 text/plain 只有一句「请用 HTML 查看」,此时改用 HTML 版本
	plainIsStub := plainRunes < 80 && utf8.RuneCountInString(htmlText) > plainRunes

	switch {
	case plainText != "" && !looksLikeHTML(plainText) && !plainIsStub:
		out.Text = normalizeBody(plainText)
	case htmlText != "":
		out.Text = normalizeBody(htmlText)
	case plainText != "":
		out.Text = normalizeBody(stripHTML(plainText))
	}
	if out.Text != "" && looksLikeCSS(out.Text) {
		out.Text = sanitizePlainPreview(out.Text)
	}
	return out
}

// walkEntity 深度优先遍历 MIME 树,收集第一个 text/plain 与第一个 text/html 段。
func walkEntity(e *message.Entity, plain, html *strings.Builder, depth int) {
	if e == nil || depth > 8 {
		return
	}
	mediaType, params, _ := e.Header.ContentType()
	mediaType = strings.ToLower(mediaType)
	if mr := e.MultipartReader(); mr != nil {
		for {
			part, err := mr.NextPart()
			if part != nil {
				walkEntity(part, plain, html, depth+1)
			}
			if err != nil {
				// io.EOF 或截断导致的错误:停止遍历,保留已收集内容
				return
			}
			if plain.Len() > 0 && html.Len() > 0 {
				return
			}
		}
	}
	disp, _, _ := e.Header.ContentDisposition()
	if strings.EqualFold(disp, "attachment") {
		return
	}
	if mediaType == "" {
		mediaType = "text/plain"
	}
	switch mediaType {
	case "text/plain":
		if plain.Len() == 0 {
			plain.WriteString(readPart(e, params))
		}
	case "text/html":
		if html.Len() == 0 {
			html.WriteString(readPart(e, params))
		}
	case "message/rfc822":
		if inner, err := message.Read(e.Body); inner != nil || err == nil {
			walkEntity(inner, plain, html, depth+1)
		}
	}
}

// readPart 读取一个文本段。go-message 已经按 Content-Transfer-Encoding 与 charset 解码;
// 截断导致的错误会被忽略,返回已读取的部分。
func readPart(e *message.Entity, params map[string]string) string {
	data, _ := io.ReadAll(io.LimitReader(e.Body, partMaxBytes))
	if len(data) == 0 {
		return ""
	}
	if !utf8.Valid(data) {
		// 字符集未知/声明错误时尝试按 GB18030 解码(国内邮件最常见)
		if r, err := message.CharsetReader("gb18030", bytes.NewReader(data)); err == nil {
			if decoded, err := io.ReadAll(r); err == nil && utf8.Valid(decoded) {
				return string(decoded)
			}
		}
		return strings.ToValidUTF8(string(data), "")
	}
	_ = params
	return string(data)
}

func looksLikeHTML(s string) bool {
	head := strings.ToLower(s)
	if len(head) > 2048 {
		head = head[:2048]
	}
	return strings.Contains(head, "<html") || strings.Contains(head, "<body") ||
		(strings.Contains(head, "<div") && strings.Contains(head, "</div>")) ||
		(strings.Contains(head, "<table") && strings.Contains(head, "</td>"))
}

// normalizeBody 规整正文:去掉零宽字符,合并空白与多余空行。
func normalizeBody(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = invisibleRE.ReplaceAllString(s, "")
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(spaceRunRE.ReplaceAllString(line, " "))
	}
	s = strings.Join(lines, "\n")
	s = blankLinesRE.ReplaceAllString(s, "\n\n")
	s = strings.TrimSpace(s)
	if len(s) > BodyMaxBytes {
		s = strings.ToValidUTF8(s[:BodyMaxBytes], "") + "\n\n…(正文过长,已截断)"
	}
	return s
}

// makePreview 生成列表摘要:单行、去链接、限制长度。
func makePreview(text string) string {
	if text == "" {
		return ""
	}
	s := urlRE.ReplaceAllString(text, " ")
	s = emptyBrRE.ReplaceAllString(s, " ")
	s = strings.Join(strings.Fields(s), " ")
	s = strings.Trim(s, " -_=*|·")
	return truncateRunes(s, PreviewMaxRunes)
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n])) + "…"
}

// decodeHeader 解码 RFC 2047 编码的邮件头(如 =?UTF-8?B?xxx?=)。
func decodeHeader(s string) string {
	if s == "" {
		return ""
	}
	dec := mime.WordDecoder{CharsetReader: message.CharsetReader}
	out, err := dec.DecodeHeader(s)
	if err != nil {
		return s
	}
	return out
}

