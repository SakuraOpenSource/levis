package plugin

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type archiveEntry struct {
	name string
	mode os.FileMode
	data string
}

func makeArchive(t *testing.T, entries ...archiveEntry) io.Reader {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, entry := range entries {
		h := &zip.FileHeader{Name: entry.name, Method: zip.Store}
		h.SetMode(entry.mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, entry.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(buf.Bytes())
}

func validArchive(t *testing.T, extra ...archiveEntry) io.Reader {
	t.Helper()
	entries := []archiveEntry{
		{name: "demo/plugin", mode: 0o755, data: "binary"},
		{name: "demo/frontend/index.html", mode: 0o644, data: "<!doctype html>"},
	}
	return makeArchive(t, append(entries, extra...)...)
}

func TestInstallArchiveSuccessAndPermissions(t *testing.T) {
	dataDir := t.TempDir()
	id, err := InstallArchive(dataDir, validArchive(t))
	if err != nil {
		t.Fatalf("安装应成功: %v", err)
	}
	if id != "demo" {
		t.Fatalf("ID = %q, want demo", id)
	}
	if _, err := os.Stat(filepath.Join(Root(dataDir), "demo", "frontend", "index.html")); err != nil {
		t.Fatalf("前端入口未安装: %v", err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(Root(dataDir), "demo", execName()))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Errorf("可执行文件权限 = %#o, want 0700", got)
		}
	}
}

func TestInstallArchiveRejectsUnsafeAndMalformedLayout(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{"traversal", "demo/../escape"},
		{"absolute", "/demo/plugin"},
		{"backslash", "demo\\plugin"},
		{"dot component", "demo/./plugin"},
		{"empty component", "demo//plugin"},
		{"multiple roots", "other/extra"},
		{"missing executable", "demo/frontend/other.html"},
		{"missing frontend", "demo/other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dataDir := t.TempDir()
			var r io.Reader
			switch tt.name {
			case "multiple roots":
				r = makeArchive(t, archiveEntry{name: "demo/plugin", mode: 0o755}, archiveEntry{name: tt.path, mode: 0o644})
			case "missing executable":
				r = makeArchive(t, archiveEntry{name: tt.path, mode: 0o644})
			case "missing frontend":
				r = makeArchive(t, archiveEntry{name: tt.path, mode: 0o755})
			default:
				r = makeArchive(t, archiveEntry{name: tt.path, mode: 0o644})
			}
			if _, err := InstallArchive(dataDir, r); err == nil {
				t.Fatal("不安全或不完整归档应被拒绝")
			}
			if _, err := os.Stat(filepath.Join(Root(dataDir), "demo")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("失败安装不应留下目标目录，err=%v", err)
			}
		})
	}
}

func TestInstallArchiveRejectsDuplicateAndConflictingEntries(t *testing.T) {
	cases := [][]archiveEntry{
		{{"demo/plugin", 0o755, "a"}, {"demo/plugin", 0o755, "b"}},
		{{"demo/plugin", 0o755, "a"}, {"demo/plugin/", os.ModeDir | 0o700, ""}},
		{{"demo", 0o644, "file"}, {"demo/plugin", 0o755, "a"}},
	}
	for i, entries := range cases {
		t.Run(strings.Join([]string{"case", string(rune('0' + i))}, "-"), func(t *testing.T) {
			if _, err := InstallArchive(t.TempDir(), makeArchive(t, entries...)); err == nil {
				t.Fatal("重复或冲突条目应被拒绝")
			}
		})
	}
}

func TestInstallArchiveRejectsSymlinkAndSpecialEntries(t *testing.T) {
	for _, mode := range []os.FileMode{os.ModeSymlink | 0o777, os.ModeNamedPipe | 0o600, os.ModeSocket | 0o600} {
		t.Run(mode.String(), func(t *testing.T) {
			entries := []archiveEntry{{"demo/plugin", mode, "target"}, {"demo/frontend/index.html", 0o644, "x"}}
			if _, err := InstallArchive(t.TempDir(), makeArchive(t, entries...)); err == nil {
				t.Fatal("符号链接或特殊文件应被拒绝")
			}
		})
	}
}

func TestInstallArchiveExistingIDAndCleanup(t *testing.T) {
	dataDir := t.TempDir()
	if err := EnsureRoot(dataDir); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(Root(dataDir), "demo")
	if err := os.Mkdir(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallArchive(dataDir, validArchive(t)); err == nil {
		t.Fatal("已有 ID 应被拒绝")
	}
	entries, err := os.ReadDir(Root(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".install-") {
			t.Errorf("失败安装留下临时目录 %q", entry.Name())
		}
	}
}

func TestInstallArchiveCreatesMissingLayout(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "nested", "data")
	if _, err := InstallArchive(dataDir, validArchive(t)); err != nil {
		t.Fatalf("应创建缺失的数据/插件目录: %v", err)
	}
}
