package handler

import (
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/gorilla/websocket"

	"github.com/SakuraOpenSource/levis/internal/httpx"
)

// checkWSOrigin 校验 WebSocket 握手来源，防止 Cookie 会话被跨站页面盗用。
// 浏览器发起 ws 握手一定会带 Origin；两者都缺席视为非浏览器客户端，放行。
// 站点常经反代（宝塔/Nginx）转发到 127.0.0.1:8080，反代可能改写 Host
// （如 proxy_set_header Host localhost），此时 Origin 与 r.Host 必然不等，
// 全部 ws 握手会被误杀（生产 403 的根因）。因此除 r.Host 外，同时接受
// 可信反代头 X-Forwarded-Host（Nginx 应设为 $host，多级代理为逗号分隔，
// 任一匹配即放行）。
func checkWSOrigin(r *http.Request) bool {
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" {
			return false
		}
		return matchWSHost(r, u.Host)
	}
	if referer := r.Header.Get("Referer"); referer != "" {
		u, err := url.Parse(referer)
		if err != nil || u.Host == "" {
			return false
		}
		return matchWSHost(r, u.Host)
	}
	return true
}

// matchWSHost 报告 host 是否与请求 Host 或 X-Forwarded-Host 之一相同。
// 反代头由站点自己的 Nginx 设置，视为可信。
func matchWSHost(r *http.Request, host string) bool {
	hosts := []string{r.Host}
	if forwarded := r.Header.Get("X-Forwarded-Host"); forwarded != "" {
		for _, candidate := range strings.Split(forwarded, ",") {
			if candidate = strings.TrimSpace(candidate); candidate != "" {
				hosts = append(hosts, candidate)
			}
		}
	}
	for _, candidate := range hosts {
		if strings.EqualFold(candidate, host) {
			return true
		}
	}
	return false
}

// vncUpgrader 与 Virtualis 主控保持一致：noVNC 走 binary 子协议。
// 来源由 checkWSOrigin 校验：同源（Origin/Referer 与 Host 一致）或两者都缺席才放行。
var vncUpgrader = websocket.Upgrader{
	ReadBufferSize:  32 << 10,
	WriteBufferSize: 32 << 10,
	Subprotocols:    []string{"binary"},
	CheckOrigin:     func(r *http.Request) bool { return checkWSOrigin(r) },
}

// ServiceVNC 返回上游主机的 VNC 接入信息（是否可用）。
// 真正的屏幕通道走同源的 ServiceVNCWebSocket 代理，避免浏览器直连上游
// 时的跨域与混合内容问题。
// 部分上游（如魔方财务）只给页面控制台地址 viewer_url：此时同样 available，
// 前端新窗口打开外链，不走 ws 中继（见 ServiceVNCWebSocket 的 400 分支）。
func (h *Handler) ServiceVNC(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	vnc, err := h.billing().ServiceVNC(httpx.CurrentUserID(c), id)
	if err != nil {
		respond(c, nil, err)
		return
	}
	OK(c, gin.H{"available": vnc.GetAvailable(), "message": vnc.GetMessage(), "viewer_url": vnc.GetViewerUrl()})
}

// ServiceVNCWebSocket 把浏览器的 noVNC 连接经主程序中继到上游。
// 流程：查服务归属 → 插件领一次性短票 → 直连上游 ws-ticket 通道 → 双向拷贝。
// 短票在领用后 120 秒内有效，建连即核销；浏览器侧全程只认本站同源地址。
func (h *Handler) ServiceVNCWebSocket(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	userID := httpx.CurrentUserID(c)
	vnc, err := h.billing().ServiceVNC(userID, id)
	if err != nil {
		respond(c, nil, err)
		return
	}
	if vnc.GetWsUrl() == "" {
		// 页面控制台型上游（如魔方财务）没有可中继的 ws 通道：明确告诉调用方
		// 用外链打开，而不是去拨一个空地址。
		if vnc.GetViewerUrl() != "" {
			BadRequest(c, "上游 VNC 为页面控制台，请使用外部链接打开")
			return
		}
		Internal(c, "上游未返回 VNC 通道地址")
		return
	}
	if !checkWSOrigin(c.Request) {
		httpx.Forbidden(c, "跨域 WebSocket 请求被拒绝")
		return
	}
	browser, err := vncUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return // 升级失败时响应已写出
	}
	defer browser.Close()
	browser.SetReadLimit(16 << 20)

	dialer := websocket.Dialer{
		ReadBufferSize: 32 << 10, WriteBufferSize: 32 << 10,
		HandshakeTimeout: 10 * time.Second, Subprotocols: []string{"binary"},
	}
	upstream, resp, err := dialer.Dial(vnc.GetWsUrl(), nil)
	if err != nil {
		log.Printf("VNC 中继 服务 %d 连接上游失败: %v", id, err)
		_ = browser.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "连接上游失败"),
			time.Now().Add(time.Second))
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return
	}
	defer upstream.Close()
	log.Printf("VNC 中继 服务 %d 已建立: 用户 %d", id, userID)

	var bytesToUpstream, bytesToBrowser atomic.Uint64
	result := make(chan error, 2)
	go func() { // 浏览器 → 上游
		for {
			messageType, reader, readErr := browser.NextReader()
			if readErr != nil {
				result <- readErr
				return
			}
			writer, writeErr := upstream.NextWriter(messageType)
			if writeErr != nil {
				result <- writeErr
				return
			}
			n, copyErr := io.Copy(writer, reader)
			bytesToUpstream.Add(uint64(n))
			_ = writer.Close()
			if copyErr != nil {
				result <- copyErr
				return
			}
		}
	}()
	go func() { // 上游 → 浏览器
		for {
			messageType, reader, readErr := upstream.NextReader()
			if readErr != nil {
				result <- readErr
				return
			}
			writer, writeErr := browser.NextWriter(messageType)
			if writeErr != nil {
				result <- writeErr
				return
			}
			n, copyErr := io.Copy(writer, reader)
			bytesToBrowser.Add(uint64(n))
			_ = writer.Close()
			if copyErr != nil {
				result <- copyErr
				return
			}
		}
	}()
	closeErr := <-result
	_ = browser.Close()
	_ = upstream.Close()
	<-result
	log.Printf("VNC 中继 服务 %d 断开: %v（上行 %d B / 下行 %d B）",
		id, closeErr, bytesToUpstream.Load(), bytesToBrowser.Load())
}
