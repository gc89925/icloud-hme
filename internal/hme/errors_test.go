package hme

import (
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

func TestParseSetCookiePreservesQuotesAndExpiry(t *testing.T) {
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	name, value, expired := parseSetCookie(`X-APPLE-WEBAUTH-USER="v=1:s=0:d=123";Path=/;Domain=.icloud.com;Secure;HttpOnly`, now)
	if name != "X-APPLE-WEBAUTH-USER" || value != `"v=1:s=0:d=123"` || expired {
		t.Fatalf("got %q %q %v", name, value, expired)
	}
	_, value, expired = parseSetCookie(`X-APPLE-WEBAUTH-TOKEN=;Expires=Thu, 01-Jan-1970 00:00:01 GMT;Path=/;Domain=.icloud.com`, now)
	if value != "" || !expired {
		t.Fatalf("expected expired empty cookie, got %q %v", value, expired)
	}
	_, _, expired = parseSetCookie(`A=b; Max-Age=0`, now)
	if !expired {
		t.Fatalf("Max-Age=0 should expire")
	}
}

func TestApplySetCookiesUpdatesAndDeletes(t *testing.T) {
	c := &Client{Cookies: map[string]string{"A": "1", "B": "2"}}
	c.applySetCookies([]string{`A="new"; Path=/`, `B=; Expires=Thu, 01-Jan-1970 00:00:01 GMT`, `C=3`})
	got := c.CookiesSnapshot()
	if got["A"] != `"new"` || got["C"] != "3" {
		t.Fatalf("cookies = %#v", got)
	}
	if _, ok := got["B"]; ok {
		t.Fatalf("B should be deleted: %#v", got)
	}
	if c.CookieRevision() != 1 {
		t.Fatalf("revision = %d", c.CookieRevision())
	}
	c.applySetCookies([]string{`A="new"`})
	if c.CookieRevision() != 1 {
		t.Fatalf("unchanged cookies must not bump revision")
	}
}

func TestUpstreamErrorMessages(t *testing.T) {
	cases := []struct {
		err  *UpstreamError
		want string
	}{
		{&UpstreamError{Kind: KindSessionExpired, Op: "获取别名列表", HTTPStatus: 421}, "HTTP 421"},
		{&UpstreamError{Kind: KindRateLimited, Op: "创建邮箱", AppleCode: "-41015"}, "-41015"},
		{&UpstreamError{Kind: KindUpstream, Op: "获取别名列表", HTTPStatus: 503}, "HTTP 503"},
	}
	for _, tc := range cases {
		if msg := tc.err.Message(); !strings.Contains(msg, tc.want) || !strings.HasPrefix(msg, tc.err.Op) {
			t.Fatalf("Message() = %q, want contains %q", msg, tc.want)
		}
	}
}

func TestRedactProxyCredentials(t *testing.T) {
	got := redact(`proxyconnect tcp: dial socks5://user:secret@1.2.3.4:1080 failed`)
	if strings.Contains(got, "secret") || strings.Contains(got, "user:") {
		t.Fatalf("credentials leaked: %q", got)
	}
}

func TestBusinessErrorRateLimit(t *testing.T) {
	err := businessError("创建邮箱", gjsonParse(`{"success":false,"error":{"errorCode":"-41015","errorMessage":"You have reached the limit of addresses you can create right now. Please try again later.","retryAfter":2}}`))
	ue, ok := AsUpstreamError(err)
	if !ok || ue.Kind != KindRateLimited || ue.RetryAfter != 2 {
		t.Fatalf("got %#v", err)
	}
}

func gjsonParse(s string) gjson.Result { return gjson.Parse(s) }

