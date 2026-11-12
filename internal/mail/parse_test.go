package mail

import (
	"strings"
	"testing"
)

const multipartGBK = "From: =?UTF-8?B?UVHpgq7nrrE=?= <sender@example.com>\r\n" +
	"To: user@example.com\r\n" +
	"Subject: =?gb18030?B?sOLWyrDy?=\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/alternative; boundary=\"----=_NextPart_ABC\"\r\n" +
	"\r\n" +
	"This is a multi-part message in MIME format.\r\n" +
	"\r\n" +
	"------=_NextPart_ABC\r\n" +
	"Content-Type: text/plain; charset=\"gb18030\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"0enWpMLro7oxMjM0NTYgx+u+ob/syrnTwyBodHRwczovL2V4YW1wbGUuY29tL2E/Yj1j\r\n" +
	"\r\n" +
	"------=_NextPart_ABC\r\n" +
	"Content-Type: text/html; charset=\"utf-8\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"PGh0bWw+PGJvZHk+PHA+SFRNTCDniYjmnKw8L3A+PC9ib2R5PjwvaHRtbD4=\r\n" +
	"\r\n" +
	"------=_NextPart_ABC--\r\n"

func TestParseRawMessageDecodesMultipartBase64Charset(t *testing.T) {
	got := parseRawMessage([]byte(multipartGBK))
	if !strings.Contains(got.Text, "验证码：123456") {
		t.Fatalf("Text = %q, want decoded GB18030 text/plain part", got.Text)
	}
	if strings.Contains(got.Text, "multi-part message") || strings.Contains(got.Text, "base64") {
		t.Fatalf("Text leaked raw MIME: %q", got.Text)
	}
	preview := makePreview(got.Text)
	if strings.Contains(preview, "https://") {
		t.Fatalf("preview should drop URLs, got %q", preview)
	}
}

func TestParseRawMessageHTMLOnlyQuotedPrintable(t *testing.T) {
	raw := "Subject: test\r\nContent-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n" +
		"<html><head><style>body{color:red}</style></head><body><p>=E4=BD=A0=E5=A5=BD</p><p>Line2</p></body></html>\r\n"
	got := parseRawMessage([]byte(raw))
	if got.Text != "你好\nLine2" {
		t.Fatalf("Text = %q", got.Text)
	}
}

func TestParseRawMessageTruncatedKeepsText(t *testing.T) {
	cut := multipartGBK[:strings.Index(multipartGBK, "------=_NextPart_ABC\r\nContent-Type: text/html")]
	got := parseRawMessage([]byte(cut))
	if !strings.Contains(got.Text, "123456") {
		t.Fatalf("truncated message lost text: %q", got.Text)
	}
}

func TestMakePreviewTruncates(t *testing.T) {
	long := strings.Repeat("很长的邮件内容 ", 200)
	p := makePreview(long)
	if n := len([]rune(p)); n > PreviewMaxRunes+1 {
		t.Fatalf("preview runes = %d, want <= %d", n, PreviewMaxRunes+1)
	}
	if !strings.HasSuffix(p, "…") {
		t.Fatalf("preview should end with ellipsis: %q", p)
	}
	if strings.Contains(p, "\n") {
		t.Fatalf("preview should be single line")
	}
}


func TestParseRawMessagePrefersHTMLWhenPlainIsStub(t *testing.T) {
	raw := "Subject: t\r\nContent-Type: multipart/alternative; boundary=XX\r\n\r\n--XX\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nPlease view this mail in HTML format.\r\n--XX\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>尊敬的客户，您的账户有一笔逾期款项需要处理，请于本月底前完成还款，以免影响您的个人征信记录。</p>\r\n--XX--\r\n"
	got := parseRawMessage([]byte(raw))
	if !strings.Contains(got.Text, "逾期款项") {
		t.Fatalf("Text = %q, want HTML body", got.Text)
	}
}

func TestMakePreviewDropsBracketedURLs(t *testing.T) {
	got := makePreview("点击此处 <https://example.com/x?y=1> 查看 (https://a.b/c) 完成")
	if strings.Contains(got, "<") || strings.Contains(got, "(") || strings.Contains(got, "http") {
		t.Fatalf("preview = %q", got)
	}
}
