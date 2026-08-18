package plugin

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestValidID 覆盖 ID 校验，重点是路径穿越与大小写。
func TestValidID(t *testing.T) {
	good := []string{"smtp", "smtp-mailer", "alipay2", "a", "x-1-y"}
	for _, id := range good {
		if err := ValidID(id); err != nil {
			t.Errorf("%q 应通过校验，却报错: %v", id, err)
		}
	}

	bad := []string{
		"",                              // 空
		"..",                            // 路径穿越
		"../etc",                        // 路径穿越
		"SMTP",                          // 大写
		"smtp_mailer",                   // 下划线
		"-smtp",                         // 首字符短横线
		"smtp-",                         // 尾字符短横线
		"smtp mailer",                   // 空格
		"smtp/mailer",                   // 分隔符
		"smtp.mailer",                   // 点
		strings.Repeat("a", maxIDLen+1), // 超长
	}
	for _, id := range bad {
		if err := ValidID(id); err == nil {
			t.Errorf("%q 应被拒绝", id)
		}
	}
}

// TestDiscoverEmpty 确认目录不存在时返回空而不是错误。
func TestDiscoverEmpty(t *testing.T) {
	found, err := Discover(t.TempDir())
	if err != nil {
		t.Fatalf("目录不存在不应报错: %v", err)
	}
	if len(found) != 0 {
		t.Errorf("应返回空列表，实际 %d 项", len(found))
	}
}

// TestEnsureRootPermission 确认插件根目录以 0700 创建。
func TestEnsureRootPermission(t *testing.T) {
	dataDir := t.TempDir()
	if err := EnsureRoot(dataDir); err != nil {
		t.Fatalf("创建插件目录失败: %v", err)
	}
	info, err := os.Stat(Root(dataDir))
	if err != nil {
		t.Fatalf("读取插件目录失败: %v", err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("插件目录权限应为 0700，实际 %#o", perm)
	}
}

// TestDiscoverSkipsWorldWritable 确认可被他人写入的插件被跳过。
//
// 这是本设计里最重要的一道文件系统检查：插件以主程序的身份运行，任何人
// 能改写它就等于能以主程序的权限执行任意代码。
func TestDiscoverSkipsWorldWritable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不使用 Unix 权限位")
	}
	dataDir := t.TempDir()
	dir := filepath.Join(Root(dataDir), "evil")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	exe := filepath.Join(dir, execName())
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatalf("写入文件失败: %v", err)
	}
	// WriteFile 的 mode 会被 umask 削掉，0777 落地通常是 0755，
	// 那样反而不满足「他人可写」这个前提，必须显式再设一次。
	if err := os.Chmod(exe, 0o777); err != nil {
		t.Fatalf("设置文件权限失败: %v", err)
	}

	found, err := Discover(dataDir)
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("应返回 1 项，实际 %d", len(found))
	}
	if found[0].Skipped == "" {
		t.Fatal("0777 的插件文件应被跳过")
	}
	if !strings.Contains(found[0].Skipped, "写入") {
		t.Errorf("跳过原因应说明可被写入，实际 %q", found[0].Skipped)
	}
}

// TestDiscoverSkipsWorldWritableDir 确认目录本身可被他人写入时也跳过。
//
// 只查文件不查目录是不够的：目录可写就能直接把 plugin 换成另一个文件。
func TestDiscoverSkipsWorldWritableDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不使用 Unix 权限位")
	}
	dataDir := t.TempDir()
	dir := filepath.Join(Root(dataDir), "evil")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	exe := filepath.Join(dir, execName())
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatalf("写入文件失败: %v", err)
	}
	// MkdirAll 会受 umask 影响，显式再设一次。
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatalf("设置目录权限失败: %v", err)
	}

	found, err := Discover(dataDir)
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if len(found) != 1 || found[0].Skipped == "" {
		t.Fatalf("可写目录应被跳过，实际 %+v", found)
	}
	if !strings.Contains(found[0].Skipped, "插件目录") {
		t.Errorf("跳过原因应指出是目录问题，实际 %q", found[0].Skipped)
	}
}

// TestDiscoverSkipsMissingExecutable 确认缺少可执行文件时给出明确原因。
func TestDiscoverSkipsMissingExecutable(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(Root(dataDir), "empty"), 0o700); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	found, err := Discover(dataDir)
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("应返回 1 项，实际 %d", len(found))
	}
	if !strings.Contains(found[0].Skipped, execName()) {
		t.Errorf("跳过原因应指出缺少哪个文件，实际 %q", found[0].Skipped)
	}
}

// TestDiscoverSkipsNonExecutable 确认没有执行位的文件被跳过。
func TestDiscoverSkipsNonExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不使用 Unix 权限位")
	}
	dataDir := t.TempDir()
	dir := filepath.Join(Root(dataDir), "noexec")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, execName()), []byte("x"), 0o600); err != nil {
		t.Fatalf("写入文件失败: %v", err)
	}

	found, err := Discover(dataDir)
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if len(found) != 1 || !strings.Contains(found[0].Skipped, "chmod +x") {
		t.Errorf("应提示加执行权限，实际 %+v", found)
	}
}

// TestDiscoverRejectsBadDirName 确认非法目录名被拒绝且带原因。
func TestDiscoverRejectsBadDirName(t *testing.T) {
	dataDir := t.TempDir()
	for _, name := range []string{"Bad_Name", "with space"} {
		if err := os.MkdirAll(filepath.Join(Root(dataDir), name), 0o700); err != nil {
			t.Fatalf("创建目录失败: %v", err)
		}
	}
	found, err := Discover(dataDir)
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if len(found) != 2 {
		t.Fatalf("应返回 2 项，实际 %d", len(found))
	}
	for _, f := range found {
		if f.Skipped == "" {
			t.Errorf("%q 应被跳过", f.ID)
		}
	}
}

// TestDiscoverIgnoresLooseFiles 确认根目录下的散装文件被忽略。
func TestDiscoverIgnoresLooseFiles(t *testing.T) {
	dataDir := t.TempDir()
	if err := EnsureRoot(dataDir); err != nil {
		t.Fatalf("创建插件目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(Root(dataDir), "readme.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("写入文件失败: %v", err)
	}
	found, err := Discover(dataDir)
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if len(found) != 0 {
		t.Errorf("散装文件应被忽略，实际返回 %+v", found)
	}
}

// TestRingBuffer 覆盖日志环形缓冲的绕回。
func TestRingBuffer(t *testing.T) {
	r := newRing(3)
	if got := r.lines(); len(got) != 0 {
		t.Errorf("空缓冲应返回空，实际 %v", got)
	}
	r.push("a")
	r.push("b")
	if got := r.lines(); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("未满时应按顺序返回，实际 %v", got)
	}
	r.push("c")
	r.push("d")
	got := r.lines()
	if len(got) != 3 || got[0] != "b" || got[2] != "d" {
		t.Errorf("绕回后应保留最近三行 b,c,d，实际 %v", got)
	}
}
