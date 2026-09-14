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
// Origin 存在则必须与请求 Host 同源，否则拒绝；Origin 缺席时再看 Referer，同理。
func checkWSOrigin(r *http.Request) bool {
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" {
			return false
		}
		return strings.EqualFold(u.Host, r.Host)
	}
	if referer := r.Header.Get("Referer"); referer != "" {
		u, err := url.Parse(referer)
		if err != nil || u.Host == "" {
			return false
		}
		return strings.EqualFold(u.Host, r.Host)
	}
	return true
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
	OK(c, gin.H{"available": vnc.GetAvailable(), "message": vnc.GetMessage()})
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
