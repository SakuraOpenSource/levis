package handler

import (
	"context"
	"fmt"
	"github.com/SakuraOpenSource/levis/internal/httpx"
	"github.com/SakuraOpenSource/levis/internal/plugin"
	"github.com/SakuraOpenSource/levis/internal/service"
	"github.com/gin-gonic/gin"
	"io"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"
	"unicode"
)

func (h *Handler) changes() *service.ServiceChangeService {
	if h.plugins == nil {
		return service.NewServiceChangeService(h.db(), nil)
	}
	return service.NewServiceChangeService(h.db(), h.plugins)
}
func (h *Handler) hostFeatures() *service.HostFeatureService {
	if h.plugins == nil {
		return service.NewHostFeatureService(h.db(), nil)
	}
	return service.NewHostFeatureService(h.db(), h.plugins)
}
func (h *Handler) ServiceChangeOptions(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	out, e := h.changes().Options(c.Request.Context(), httpx.CurrentUserID(c), id)
	respond(c, gin.H{"items": out}, e)
}
func (h *Handler) ServiceChangePreview(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	var in service.ChangeInput
	if !bindJSON(c, &in) {
		return
	}
	out, e := h.changes().Preview(c.Request.Context(), httpx.CurrentUserID(c), id, in)
	respond(c, out, e)
}
func (h *Handler) ServiceChange(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	var in service.ChangeInput
	if !bindJSON(c, &in) {
		return
	}
	out, e := h.changes().Change(c.Request.Context(), httpx.CurrentUserID(c), id, in)
	respond(c, out, e)
}
func (h *Handler) ServiceChangeRetry(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	cid, ok := IDParam(c, "changeID")
	if !ok {
		return
	}
	out, e := h.changes().Retry(c.Request.Context(), httpx.CurrentUserID(c), id, cid)
	respond(c, out, e)
}
func (h *Handler) ServiceChanges(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	out, e := h.changes().List(httpx.CurrentUserID(c), id)
	respond(c, gin.H{"items": out}, e)
}
func (h *Handler) ServiceHostOperation(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	user := httpx.CurrentUser(c)
	admin := user != nil && user.IsAdmin()
	var payload []byte
	var e error
	if c.Request.Method == "POST" {
		payload, e = io.ReadAll(io.LimitReader(c.Request.Body, 65537))
		if e != nil || len(payload) > 65536 {
			BadRequest(c, "操作参数过大或读取失败")
			return
		}
	} else {
		fields := map[string]string{}
		for k, v := range c.Request.URL.Query() {
			if len(v) != 1 {
				BadRequest(c, "重复参数")
				return
			}
			fields[k] = v[0]
		}
		if len(fields) > 0 { // GET parameters currently only accept numeric agent_id.
			if len(fields) != 1 || fields["agent_id"] == "" {
				BadRequest(c, "不允许的查询参数")
				return
			}
			for _, r := range fields["agent_id"] {
				if r < '0' || r > '9' {
					BadRequest(c, "节点 ID 无效")
					return
				}
			}
			payload = []byte(fmt.Sprintf(`{"agent_id":%s}`, fields["agent_id"]))
		}
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Hour)
	defer cancel()
	out, e := h.hostFeatures().Operation(ctx, httpx.CurrentUserID(c), id, admin, c.Request.Method, c.Param("action"), payload)
	respond(c, out, e)
}
func (h *Handler) DownloadServiceBackup(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	bid, ok := IDParam(c, "backupID")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Hour)
	defer cancel()
	stream, e := h.hostFeatures().Download(ctx, httpx.CurrentUserID(c), id, uint64(bid))
	if e != nil {
		respond(c, nil, e)
		return
	}
	if e = streamBackupResponse(c.Writer, stream); e != nil {
		respond(c, nil, e)
	}
}

// Once bytes have been emitted, failures must terminate HTTP framing rather than
// append JSON or send a normal EOF. The server recovery middleware preserves this.
func streamBackupResponse(w http.ResponseWriter, stream plugin.BackupStream) error {
	if stream == nil {
		return fmt.Errorf("上游未提供备份流")
	}
	written := false
	total := int64(0)
	fail := func(e error) error {
		if written {
			panic(http.ErrAbortHandler)
		}
		return e
	}
	for {
		chunk, e := stream.Recv()
		if e == io.EOF {
			if total == 0 {
				return fail(fmt.Errorf("空备份流"))
			}
			return nil
		}
		if e != nil {
			return fail(e)
		}
		if chunk == nil || len(chunk.Data) > 64*1024 {
			return fail(fmt.Errorf("无效的备份块"))
		}
		if !written {
			filename := path.Base(strings.ReplaceAll(chunk.Filename, "\\", "/"))
			filename = strings.Map(func(r rune) rune {
				if unicode.IsControl(r) {
					return -1
				}
				return r
			}, filename)
			if filename == "" || filename == "." || filename == "/" || len(filename) > 255 {
				filename = "backup.tar"
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Content-Type-Options", "nosniff")
		}
		if len(chunk.Data) == 0 {
			continue
		}
		n, e := w.Write(chunk.Data)
		written = true
		total += int64(n)
		if e != nil {
			return fail(e)
		}
		if n != len(chunk.Data) {
			return fail(io.ErrShortWrite)
		}
		if flush, ok := w.(http.Flusher); ok {
			flush.Flush()
		}
	}
}
