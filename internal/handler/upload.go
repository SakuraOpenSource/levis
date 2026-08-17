package handler

import (
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/SakuraOpenSource/levis/internal/service"
)

// formFiles 把 multipart 表单里 field 字段下的文件转成 service.Upload。
//
// 表单本身不是 multipart（或超过了请求体上限）时写回 400 并返回 false。
// 字段缺失不算错误：附件本来就是可选的。
func formFiles(c *gin.Context, field string) ([]service.Upload, bool) {
	form, err := c.MultipartForm()
	if err != nil {
		// MaxBytesReader 掐断请求体时也走这里，因此提示要把两种可能都涵盖。
		BadRequest(c, "上传内容过大或表单格式不正确")
		return nil, false
	}
	return wrapFiles(form.File[field]), true
}

// wrapFiles 把 multipart 文件头转成 service.Upload。
func wrapFiles(headers []*multipart.FileHeader) []service.Upload {
	if len(headers) == 0 {
		return nil
	}
	out := make([]service.Upload, 0, len(headers))
	for _, header := range headers {
		out = append(out, wrapFile(header))
	}
	return out
}

// wrapFile 把单个 multipart 文件头转成 service.Upload。
//
// 只包一层惰性 Open，不在这里读内容：service 才知道该不该收、上限是多少。
func wrapFile(header *multipart.FileHeader) service.Upload {
	return service.Upload{
		FileName: header.Filename,
		Size:     header.Size,
		Open: func() (io.ReadCloser, error) {
			return header.Open()
		},
	}
}

// formFile 取出表单里 field 字段下的单个文件。字段缺失时返回零值 Upload
// （其 Open 为 nil，由 service 判定是否必填）。
func formFile(c *gin.Context, field string) (service.Upload, bool) {
	form, err := c.MultipartForm()
	if err != nil {
		BadRequest(c, "上传内容过大或表单格式不正确")
		return service.Upload{}, false
	}
	headers := form.File[field]
	if len(headers) == 0 {
		return service.Upload{}, true
	}
	return wrapFile(headers[0]), true
}

// sendFile 把打开的文件下发给客户端，随后关闭它。
//
// disposition 传 "attachment" 或 "inline"。工单附件一律 attachment：上传一个
// HTML 文件不能变成同源脚本。
func sendFile(c *gin.Context, file *os.File, mime, fileName, disposition string) {
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		Internal(c, "")
		return
	}
	c.Header("Content-Type", mime)
	c.Header("Content-Disposition", contentDisposition(disposition, fileName))
	// 用户上传的内容不进任何中间缓存：证件照尤其不该被代理留下副本。
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	// ServeContent 负责 Range 与条件请求；Content-Type 已设，它不会再猜。
	http.ServeContent(c.Writer, c.Request, "", info.ModTime(), file)
}

// contentDisposition 构造带非 ASCII 文件名的 Content-Disposition 头。
//
// 同时给出 ASCII 回退与 RFC 5987 的 filename*：中文文件名若只写 filename，
// 到了浏览器就是一串乱码。
func contentDisposition(kind, name string) string {
	if name == "" {
		return kind
	}
	return fmt.Sprintf(`%s; filename="%s"; filename*=UTF-8''%s`,
		kind, asciiFallback(name), url.PathEscape(name))
}

// asciiFallback 把非 ASCII 字符替换为下划线，供不认 filename* 的客户端使用。
func asciiFallback(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			b.WriteByte('_')
			continue
		}
		b.WriteRune(r)
	}
	if b.Len() == 0 {
		return "file"
	}
	return b.String()
}
