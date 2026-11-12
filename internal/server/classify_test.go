package server

import (
	"net/http"
	"strings"
	"testing"

	"icloud-hme/internal/hme"
)

func TestClassifyUpstreamErrKinds(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
		substr string
	}{
		{&hme.UpstreamError{Kind: hme.KindSessionExpired, Op: "获取别名列表", HTTPStatus: 421}, http.StatusUnauthorized, "UPSTREAM_UNAUTHORIZED", "HTTP 421"},
		{&hme.UpstreamError{Kind: hme.KindRateLimited, Op: "创建邮箱", AppleCode: "-41015"}, http.StatusTooManyRequests, "UPSTREAM_RATE_LIMITED", "-41015"},
		{&hme.UpstreamError{Kind: hme.KindNetwork, Op: "获取别名列表"}, http.StatusGatewayTimeout, "UPSTREAM_UNREACHABLE", "无法连接"},
		{&hme.UpstreamError{Kind: hme.KindUpstream, Op: "获取别名列表", HTTPStatus: 503}, http.StatusBadGateway, "UPSTREAM_FAILURE", "HTTP 503"},
	}
	for _, tc := range cases {
		be := classifyUpstreamErr("获取别名列表失败", tc.err)
		if be.Status != tc.status || be.Code != tc.code || !strings.Contains(be.Message, tc.substr) {
			t.Fatalf("classifyUpstreamErr(%v) = %+v", tc.err, be)
		}
	}
}
