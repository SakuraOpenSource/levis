// Package storage 管理用户上传文件的落盘与读取。
//
// 文件存在 <dataDir>/uploads 下，数据库只记元数据（原名、大小、MIME 与相对
// 路径）。之所以不塞进数据库：20 MiB 的行会把库撑大，MySQL 还要调
// max_allowed_packet，而 GORM 读写 BLOB 会把整块搬进内存。
//
// 落盘文件名一律由服务端随机生成，原始文件名绝不参与路径拼接 —— 这一条同时
// 挡掉了路径穿越、字符编码问题与同名覆盖，原名只用于展示。
package storage

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrTooLarge 表示上传内容超过了调用方给定的上限。
var ErrTooLarge = errors.New("文件超过大小限制")

// ErrEmpty 表示上传内容为空。
var ErrEmpty = errors.New("文件内容为空")

// ErrBadPath 表示相对路径不合法或指向 uploads 之外。
var ErrBadPath = errors.New("文件路径不合法")

// 目录与文件权限。上传内容含身份证照片这类敏感材料，与 config.json 同口径
// 收紧到仅所有者可访问。
const (
	dirPerm  = 0o700
	filePerm = 0o600
)

// sniffLen 是嗅探 MIME 所需的字节数，与 http.DetectContentType 的要求一致。
const sniffLen = 512

// Store 是以某个目录为根的文件存储。
type Store struct {
	root string
}

// New 构造以 <dataDir>/uploads 为根的 Store。
//
// 只依赖数据目录，与数据库无关，因此未安装状态下也能构造。
func New(dataDir string) *Store {
	return &Store{root: filepath.Join(dataDir, "uploads")}
}

// Root 返回存储根目录。
func (s *Store) Root() string { return s.root }

// Save 把 r 的内容写入 category 分类下，返回相对路径、字节数与嗅探出的 MIME。
//
// limit 为允许的最大字节数：多读一个字节来判断是否超限，因此恰好等于 limit
// 的内容可以通过。超限或写入失败时，已落盘的部分文件会被删掉，不留垃圾。
//
// MIME 由内容前 512 字节嗅探得出，不采信客户端声明的 Content-Type —— 那是
// 请求里最容易伪造的一项。
func (s *Store) Save(category string, r io.Reader, limit int64) (relPath string, size int64, mime string, err error) {
	if err := validSegment(category); err != nil {
		return "", 0, "", err
	}

	now := time.Now().UTC()
	// 按年月分目录：单目录堆几十万个文件后，很多文件系统的查找会明显变慢。
	dir := filepath.Join(category, now.Format("2006"), now.Format("01"))
	name, err := randomName()
	if err != nil {
		return "", 0, "", err
	}
	relPath = filepath.Join(dir, name)

	if err := os.MkdirAll(filepath.Join(s.root, dir), dirPerm); err != nil {
		return "", 0, "", fmt.Errorf("创建上传目录失败: %w", err)
	}

	abs := filepath.Join(s.root, relPath)
	file, err := os.OpenFile(abs, os.O_WRONLY|os.O_CREATE|os.O_EXCL, filePerm)
	if err != nil {
		return "", 0, "", fmt.Errorf("创建文件失败: %w", err)
	}
	// 出错时清掉半个文件；成功路径上 cleanup 会被置为 nil。
	cleanup := func() {
		file.Close()
		os.Remove(abs)
	}
	defer func() {
		if cleanup != nil {
			cleanup()
		}
	}()

	// 先取够嗅探所需的字节，再把它接回流的前面原样写出去，
	// 这样嗅探不必把整个文件读进内存。
	head := make([]byte, sniffLen)
	headLen, err := io.ReadFull(r, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", 0, "", fmt.Errorf("读取上传内容失败: %w", err)
	}
	head = head[:headLen]
	if headLen == 0 {
		return "", 0, "", ErrEmpty
	}
	mime = http.DetectContentType(head)

	// 多读一个字节：读到 limit+1 说明源里还有内容，即超限。
	body := io.MultiReader(bytes.NewReader(head), r)
	written, err := io.Copy(file, io.LimitReader(body, limit+1))
	if err != nil {
		return "", 0, "", fmt.Errorf("写入文件失败: %w", err)
	}
	if written > limit {
		return "", 0, "", ErrTooLarge
	}
	if err := file.Close(); err != nil {
		return "", 0, "", fmt.Errorf("关闭文件失败: %w", err)
	}

	cleanup = nil
	// 统一用斜杠存库：Windows 上落盘的反斜杠路径若直接入库，换到 Linux
	// 部署时就取不出来了。
	return filepath.ToSlash(relPath), written, mime, nil
}

// Open 打开一个此前由 Save 写入的文件。
//
// relPath 虽然来自本系统的数据库，仍按不可信输入处理：库里的值有可能被
// 其他途径写坏，一次越界读取就能把 config.json 交出去。
func (s *Store) Open(relPath string) (*os.File, error) {
	abs, err := s.resolve(relPath)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(abs)
	if err != nil {
		return nil, err
	}
	return file, nil
}

// Remove 删除文件。文件已不存在时视为成功 —— 调用方要的是「最终不在」。
func (s *Store) Remove(relPath string) error {
	abs, err := s.resolve(relPath)
	if err != nil {
		return err
	}
	if err := os.Remove(abs); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// RemoveAll 批量删除，忽略单个失败并返回第一个错误。
//
// 用于删除用户这类批量清理：某个文件删不掉不该让其余文件留在盘上。
func (s *Store) RemoveAll(relPaths []string) error {
	var first error
	for _, p := range relPaths {
		if p == "" {
			continue
		}
		if err := s.Remove(p); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// resolve 把相对路径转换为根目录下的绝对路径，并确认没有越界。
func (s *Store) resolve(relPath string) (string, error) {
	if relPath == "" || filepath.IsAbs(relPath) || strings.ContainsRune(relPath, '\x00') {
		return "", ErrBadPath
	}
	// 反斜杠在 Windows 上也是分隔符，先归一化再判断，避免 ..\ 绕过检查。
	clean := filepath.Clean(filepath.FromSlash(strings.ReplaceAll(relPath, `\`, "/")))
	if clean == "." || filepath.IsAbs(clean) {
		return "", ErrBadPath
	}

	abs := filepath.Join(s.root, clean)
	// Clean 之后再用 Rel 复核一次：仅检查 ".." 子串会漏掉编码变体，
	// 而这里是拿最终路径与根目录做实际比较。
	rel, err := filepath.Rel(s.root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", ErrBadPath
	}
	return abs, nil
}

// validSegment 校验分类名：只允许小写字母、数字与短横线。
func validSegment(name string) error {
	if name == "" || len(name) > 32 {
		return ErrBadPath
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			return ErrBadPath
		}
	}
	return nil
}

// randomName 生成 32 位十六进制的随机文件名。
func randomName() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成文件名失败: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
