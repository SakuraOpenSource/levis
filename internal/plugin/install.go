package plugin

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	maxArchiveCompressed = 32 << 20
	maxArchiveExpanded   = 128 << 20
	maxArchiveEntries    = 1024
)

// InstallArchive installs one complete plugin package atomically.
// The archive must contain <id>/plugin and <id>/frontend/index.html.
func InstallArchive(dataDir string, r io.Reader) (string, error) {
	if err := EnsureRoot(dataDir); err != nil {
		return "", err
	}
	limited := io.LimitReader(r, maxArchiveCompressed+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return "", fmt.Errorf("读取插件归档失败: %w", err)
	}
	if int64(len(data)) > maxArchiveCompressed {
		return "", fmt.Errorf("插件归档过大（上限 %d MiB）", maxArchiveCompressed>>20)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("插件归档格式无效: %w", err)
	}
	if len(zr.File) == 0 || len(zr.File) > maxArchiveEntries {
		return "", fmt.Errorf("插件归档条目数无效（上限 %d）", maxArchiveEntries)
	}

	id, err := archiveID(zr.File)
	if err != nil {
		return "", err
	}
	root := Root(dataDir)
	if _, err := os.Lstat(filepath.Join(root, id)); err == nil {
		return "", fmt.Errorf("插件 %q 已存在", id)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("检查插件目录失败: %w", err)
	}

	stage, err := os.MkdirTemp(root, ".install-")
	if err != nil {
		return "", fmt.Errorf("创建插件临时目录失败: %w", err)
	}
	defer os.RemoveAll(stage)
	_ = os.Chmod(stage, 0o700)

	var expanded int64
	seen := make(map[string]bool, len(zr.File))
	for _, file := range zr.File {
		rel, directory, err := safeArchivePath(file.Name)
		if err != nil {
			return "", err
		}
		if rel == "" || seen[rel] {
			return "", fmt.Errorf("插件归档包含重复或空路径: %q", file.Name)
		}
		seen[rel] = true
		if !strings.HasPrefix(rel, id+"/") && rel != id {
			return "", fmt.Errorf("插件归档必须只有一个顶层目录")
		}
		if file.UncompressedSize64 > uint64(maxArchiveExpanded) || expanded > maxArchiveExpanded-int64(file.UncompressedSize64) {
			return "", fmt.Errorf("插件归档解压后过大（上限 %d MiB）", maxArchiveExpanded>>20)
		}
		expanded += int64(file.UncompressedSize64)
		if directory {
			fileType := file.Mode() & os.ModeType
			if fileType != 0 && fileType != os.ModeDir {
				return "", fmt.Errorf("插件归档包含不安全目录: %q", file.Name)
			}
			if err := os.MkdirAll(filepath.Join(stage, rel), 0o700); err != nil {
				return "", fmt.Errorf("创建插件目录失败: %w", err)
			}
			continue
		}
		if file.Mode()&os.ModeSymlink != 0 || !file.Mode().IsRegular() {
			return "", fmt.Errorf("插件归档包含不安全文件: %q", file.Name)
		}
		dest := filepath.Join(stage, rel)
		if err := ensureParent(dest, stage); err != nil {
			return "", err
		}
		in, err := file.Open()
		if err != nil {
			return "", fmt.Errorf("读取插件文件失败: %w", err)
		}
		out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			in.Close()
			return "", fmt.Errorf("创建插件文件失败: %w", err)
		}
		_, copyErr := io.CopyN(out, in, int64(file.UncompressedSize64)+1)
		closeErr := out.Close()
		_ = in.Close()
		if copyErr != nil && !errors.Is(copyErr, io.EOF) {
			return "", fmt.Errorf("解压插件文件失败: %w", copyErr)
		}
		if closeErr != nil {
			return "", fmt.Errorf("保存插件文件失败: %w", closeErr)
		}
	}

	final := filepath.Join(stage, id)
	exe := filepath.Join(final, execName())
	if info, err := os.Stat(exe); err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("插件归档缺少可执行文件 %s", execName())
	}
	frontend := filepath.Join(final, "frontend", "index.html")
	if info, err := os.Stat(frontend); err != nil || info.IsDir() {
		return "", errors.New("插件归档缺少 frontend/index.html")
	}
	if err := os.Chmod(exe, 0o700); err != nil {
		return "", fmt.Errorf("设置插件执行权限失败: %w", err)
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(exe); err != nil || info.Mode().Perm()&0o100 == 0 {
			return "", errors.New("插件可执行文件没有执行权限")
		}
	}
	if err := os.Rename(final, filepath.Join(root, id)); err != nil {
		return "", fmt.Errorf("安装插件失败: %w", err)
	}
	return id, nil
}

func archiveID(files []*zip.File) (string, error) {
	var id string
	for _, file := range files {
		rel, _, err := safeArchivePath(file.Name)
		if err != nil {
			return "", err
		}
		parts := strings.Split(rel, "/")
		if len(parts) == 0 || parts[0] == "" {
			return "", errors.New("插件归档缺少顶层目录")
		}
		if id == "" {
			id = parts[0]
		} else if id != parts[0] {
			return "", errors.New("插件归档只能包含一个插件目录")
		}
	}
	if err := ValidID(id); err != nil {
		return "", fmt.Errorf("插件 ID 无效: %w", err)
	}
	return id, nil
}

func safeArchivePath(name string) (string, bool, error) {
	if name == "" || strings.IndexByte(name, 0) >= 0 || strings.Contains(name, "\\") {
		return "", false, fmt.Errorf("插件归档路径不安全: %q", name)
	}
	directory := strings.HasSuffix(name, "/")
	pathName := strings.TrimSuffix(name, "/")
	for _, part := range strings.Split(pathName, "/") {
		if part == "" || part == "." || part == ".." {
			return "", false, fmt.Errorf("插件归档路径穿越或格式无效: %q", name)
		}
	}
	clean := path.Clean(name)
	if strings.HasPrefix(name, "/") || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false, fmt.Errorf("插件归档路径穿越: %q", name)
	}
	return clean, directory, nil
}

func ensureParent(file, root string) error {
	parent := filepath.Dir(file)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("创建插件文件目录失败: %w", err)
	}
	rel, err := filepath.Rel(root, parent)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("插件归档路径超出临时目录")
	}
	return nil
}
