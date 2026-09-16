package handler

import (
	"net/http/httptest"
	"testing"
)

// TestCheckWSOrigin 覆盖 VNC WebSocket 握手的来源校验：
// 同源放行、跨域拒绝、反代改写 Host 时以 X-Forwarded-Host 为准。
func TestCheckWSOrigin(t *testing.T) {
	cases := []struct {
		name    string
		host    string
		xfh     string // X-Forwarded-Host
		origin  string
		referer string
		want    bool
	}{
		{name: "同源直连", host: "example.com", origin: "https://example.com", want: true},
		{name: "同源带端口", host: "example.com:8080", origin: "http://example.com:8080", want: true},
		{name: "跨域拒绝", host: "example.com", origin: "https://evil.com", want: false},
		{name: "无 Origin 无 Referer 视为非浏览器", host: "example.com", want: true},
		{name: "仅 Referer 同源", host: "example.com", referer: "https://example.com/dashboard", want: true},
		{name: "仅 Referer 跨域", host: "example.com", referer: "https://evil.com/x", want: false},
		{
			// 生产 403 根因：宝塔反代 proxy_set_header Host localhost，
			// 浏览器 Origin 是真实域名，必须靠 X-Forwarded-Host 放行。
			name:   "反代改写 Host 时以 X-Forwarded-Host 为准",
			host:   "localhost",
			xfh:    "web.fallingsakura.cn",
			origin: "https://web.fallingsakura.cn",
			want:   true,
		},
		{name: "X-Forwarded-Host 不匹配仍拒绝", host: "localhost", xfh: "other.com", origin: "https://evil.com", want: false},
		{name: "X-Forwarded-Host 多级取任一", host: "localhost", xfh: "inner.local, web.fallingsakura.cn", origin: "https://web.fallingsakura.cn", want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/services/1/vnc/ws", nil)
			r.Host = tc.host
			if tc.xfh != "" {
				r.Header.Set("X-Forwarded-Host", tc.xfh)
			}
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.referer != "" {
				r.Header.Set("Referer", tc.referer)
			}
			if got := checkWSOrigin(r); got != tc.want {
				t.Fatalf("checkWSOrigin = %v, want %v", got, tc.want)
			}
		})
	}
}
