package storage

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newStore 在临时目录上构造 Store。
func newStore(t *testing.T) *Store {
	t.Helper()
	return New(t.TempDir())
}

func TestSaveAndOpen(t *testing.T) {
	store := newStore(t)
	content := []byte("hello levis")

	rel, size, mime, err := store.Save("tickets", bytes.NewReader(content), 1024)
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if size != int64(len(content)) {
		t.Errorf("size = %d，期望 %d", size, len(content))
	}
	if !strings.HasPrefix(mime, "text/plain") {
		t.Errorf("mime = %q，期望 text/plain", mime)
	}
	// 相对路径必须用斜杠，否则在 Windows 上落库、Linux 上部署就取不出来。
	if strings.Contains(rel, `\`) {
		t.Errorf("相对路径含反斜杠: %q", rel)
	}
	if !strings.HasPrefix(rel, "tickets/") {
		t.Errorf("相对路径应以分类开头: %q", rel)
	}

	file, err := store.Open(rel)
	if err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	defer file.Close()
	got, err := io.ReadAll(file)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("读回内容 = %q，期望 %q", got, content)
	}
}

// 文件权限必须是 0600：上传内容含身份证照片，同机其他用户不该读得到。
func TestSaveUsesRestrictivePermissions(t *testing.T) {
	store := newStore(t)
	rel, _, _, err := store.Save("kyc", bytes.NewReader([]byte("photo bytes")), 1024)
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	info, err := os.Stat(filepath.Join(store.Root(), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("stat 失败: %v", err)
	}
	if perm := info.Mode().Perm(); perm != filePerm {
		t.Errorf("文件权限 = %o，期望 %o", perm, filePerm)
	}
}

// 恰好等于上限应通过，多一个字节应被拒。
func TestSaveEnforcesLimit(t *testing.T) {
	store := newStore(t)
	const limit = 600 // 大于 sniffLen，确保覆盖嗅探之后的写入路径

	if _, size, _, err := store.Save("tickets", bytes.NewReader(bytes.Repeat([]byte("a"), limit)), limit); err != nil {
		t.Fatalf("恰好等于上限应通过，实际: %v", err)
	} else if size != limit {
		t.Errorf("size = %d，期望 %d", size, limit)
	}

	_, _, _, err := store.Save("tickets", bytes.NewReader(bytes.Repeat([]byte("a"), limit+1)), limit)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("超限应返回 ErrTooLarge，实际: %v", err)
	}
}

// 超限之后不能在盘上留下半个文件。
func TestSaveCleansUpAfterLimitExceeded(t *testing.T) {
	store := newStore(t)
	if _, _, _, err := store.Save("tickets", bytes.NewReader(bytes.Repeat([]byte("a"), 2048)), 1024); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("超限应返回 ErrTooLarge，实际: %v", err)
	}

	var count int
	err := filepath.WalkDir(store.Root(), func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历失败: %v", err)
	}
	if count != 0 {
		t.Errorf("超限后仍留下 %d 个文件", count)
	}
}

func TestSaveRejectsEmpty(t *testing.T) {
	store := newStore(t)
	if _, _, _, err := store.Save("tickets", bytes.NewReader(nil), 1024); !errors.Is(err, ErrEmpty) {
		t.Fatalf("空内容应返回 ErrEmpty，实际: %v", err)
	}
}

// MIME 取自内容而非调用方声明：客户端的 Content-Type 是最容易伪造的一项。
func TestSaveSniffsMimeFromContent(t *testing.T) {
	store := newStore(t)
	// 最小的合法 PNG 头。
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	_, _, mime, err := store.Save("kyc", bytes.NewReader(png), 1024)
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if mime != "image/png" {
		t.Errorf("mime = %q，期望 image/png", mime)
	}
}

func TestSaveRejectsBadCategory(t *testing.T) {
	store := newStore(t)
	for _, category := range []string{"", "../etc", "Tickets", "a/b", "tickets!", strings.Repeat("a", 33)} {
		if _, _, _, err := store.Save(category, bytes.NewReader([]byte("x")), 1024); !errors.Is(err, ErrBadPath) {
			t.Errorf("分类 %q 应返回 ErrBadPath，实际: %v", category, err)
		}
	}
}

// 核心安全用例：伪造的相对路径不能读到 uploads 之外的文件。
// 库里的值一旦被写坏，一次越界读取就能把 config.json 交出去。
func TestOpenRejectsTraversal(t *testing.T) {
	dataDir := t.TempDir()
	store := New(dataDir)

	// 在 uploads 的父目录放一个「机密」文件，模拟 config.json。
	secret := filepath.Join(dataDir, "config.json")
	if err := os.WriteFile(secret, []byte(`{"jwt_secret":"leak"}`), 0o600); err != nil {
		t.Fatalf("准备文件失败: %v", err)
	}

	cases := []string{
		"../config.json",
		"../../config.json",
		"tickets/../../config.json",
		`..\config.json`,
		"tickets/../../../etc/passwd",
		secret,       // 绝对路径
		"",           // 空
		".",          // 根本身
		"..",         // 父目录
		"a\x00.json", // NUL 截断
	}
	for _, path := range cases {
		file, err := store.Open(path)
		if err == nil {
			file.Close()
			t.Errorf("路径 %q 竟然打开成功", path)
			continue
		}
		if !errors.Is(err, ErrBadPath) {
			// 允许因文件不存在而失败，但不能是「成功读到界外文件」。
			if !errors.Is(err, os.ErrNotExist) {
				t.Errorf("路径 %q 的错误 = %v，期望 ErrBadPath 或 ErrNotExist", path, err)
			}
		}
	}
}

func TestRemove(t *testing.T) {
	store := newStore(t)
	rel, _, _, err := store.Save("tickets", bytes.NewReader([]byte("bye")), 1024)
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if err := store.Remove(rel); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if _, err := store.Open(rel); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("删除后应打不开，实际: %v", err)
	}
	// 重复删除应当是无害的：调用方要的是「最终不在」。
	if err := store.Remove(rel); err != nil {
		t.Errorf("重复删除应成功，实际: %v", err)
	}
}

func TestRemoveRejectsTraversal(t *testing.T) {
	dataDir := t.TempDir()
	store := New(dataDir)
	secret := filepath.Join(dataDir, "config.json")
	if err := os.WriteFile(secret, []byte("{}"), 0o600); err != nil {
		t.Fatalf("准备文件失败: %v", err)
	}
	if err := store.Remove("../config.json"); !errors.Is(err, ErrBadPath) {
		t.Fatalf("穿越路径应返回 ErrBadPath，实际: %v", err)
	}
	if _, err := os.Stat(secret); err != nil {
		t.Fatalf("界外文件被删掉了: %v", err)
	}
}

func TestRemoveAll(t *testing.T) {
	store := newStore(t)
	var paths []string
	for range 3 {
		rel, _, _, err := store.Save("tickets", bytes.NewReader([]byte("data")), 1024)
		if err != nil {
			t.Fatalf("保存失败: %v", err)
		}
		paths = append(paths, rel)
	}
	// 混入空串与不存在的路径，批量清理不该被它们打断。
	paths = append(paths, "", "tickets/2020/01/deadbeef")

	if err := store.RemoveAll(paths); err != nil {
		t.Fatalf("批量删除失败: %v", err)
	}
	for _, p := range paths[:3] {
		if _, err := store.Open(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s 未被删除", p)
		}
	}
}

// 同一分类下连续保存不能撞名。
func TestSaveGeneratesUniqueNames(t *testing.T) {
	store := newStore(t)
	seen := make(map[string]bool)
	for range 20 {
		rel, _, _, err := store.Save("tickets", bytes.NewReader([]byte("x")), 1024)
		if err != nil {
			t.Fatalf("保存失败: %v", err)
		}
		if seen[rel] {
			t.Fatalf("路径重复: %s", rel)
		}
		seen[rel] = true
	}
}
