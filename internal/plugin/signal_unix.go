//go:build !windows

package plugin

import (
	"os"
	"syscall"
)

// sigTerm 返回请求进程优雅退出的信号。
func sigTerm() os.Signal { return syscall.SIGTERM }
