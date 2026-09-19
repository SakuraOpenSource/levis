package handler

import (
	"net/http"
	"os"

	"github.com/gin-gonic/gin"

	"github.com/SakuraOpenSource/levis/internal/httpx"
)

// 允许作为站点图标的 MIME：位图 + SVG。SVG 是文本格式，必须白名单放行，
// 但它天然可携带脚本，仅在 image/svg+xml 下内联且站点整体有 nosniff 兜底。
var allowedIconMimes = map[string]bool{
	"image/png":                true,
	"image/x-icon":             true,
	"image/vnd.microsoft.icon": true,
	"image/svg+xml":            true,
}

const siteIconMaxBytes = 1 << 20 // 1 MiB

// AdminUploadSiteIcon 上传站点图标（multipart 字段名 file）。
// 成功后旧图标文件被删除，新路径写入 settings。
func (h *Handler) AdminUploadSiteIcon(c *gin.Context) {
	fileHeader, err := c.FormFile("file")
	if err != nil {
		Fail(c, 400, "BAD_REQUEST", "请上传图标文件")
		return
	}
	if fileHeader.Size > siteIconMaxBytes {
		Fail(c, 400, "BAD_REQUEST", "图标不能超过 1 MiB")
		return
	}
	src, err := fileHeader.Open()
	if err != nil {
		Fail(c, 400, "BAD_REQUEST", "读取上传文件失败")
		return
	}
	defer src.Close()

	relPath, _, mime, err := h.storage.Save("site-icons", src, siteIconMaxBytes)
	if err != nil {
		respond(c, nil, err)
		return
	}
	if !allowedIconMimes[mime] {
		_ = h.storage.Remove(relPath)
		Fail(c, 400, "BAD_REQUEST", "图标只支持 PNG / ICO / SVG")
		return
	}

	settings := h.settings()
	old := settings.SiteIconPath()
	if err := settings.SaveSiteIconPath(relPath); err != nil {
		_ = h.storage.Remove(relPath)
		respond(c, nil, err)
		return
	}
	if old != "" && old != relPath {
		_ = h.storage.Remove(old)
	}
	OK(c, gin.H{"path": relPath, "mime": mime})
}

// AdminRemoveSiteIcon 清除站点图标（记录与文件一并删除）。
func (h *Handler) AdminRemoveSiteIcon(c *gin.Context) {
	settings := h.settings()
	old := settings.SiteIconPath()
	if old != "" {
		_ = h.storage.Remove(old)
	}
	if err := settings.SaveSiteIconPath(""); err != nil {
		respond(c, nil, err)
		return
	}
	noContent(c)
}

// SiteIcon 面向全站（含未登录访问者）内联输出站点图标。
// 未设置时 404，前端回退到内置 favicon。
func (h *Handler) SiteIcon(c *gin.Context) {
	path := h.settings().SiteIconPath()
	if path == "" {
		httpx.NotFound(c, "未设置站点图标")
		return
	}
	file, err := h.storage.Open(path)
	if err != nil {
		httpx.NotFound(c, "未设置站点图标")
		return
	}
	defer file.Close()
	mime, err := sniffMimeIcon(file)
	if err != nil || !allowedIconMimes[mime] {
		httpx.NotFound(c, "未设置站点图标")
		return
	}
	sendFile(c, file, mime, "", "inline")
}

// sniffMimeIcon 嗅探图标 MIME 并把读取位置复位（kyc.sniffMime 在 service 包内，
// handler 包无法直接复用，这里按同样语义实现：读 512 字节 → DetectContentType → 回绕）。
func sniffMimeIcon(file *os.File) (string, error) {
	head := make([]byte, 512)
	n, err := file.Read(head)
	if err != nil && n == 0 {
		return "", err
	}
	if _, serr := file.Seek(0, 0); serr != nil {
		return "", serr
	}
	return http.DetectContentType(head[:n]), nil
}
