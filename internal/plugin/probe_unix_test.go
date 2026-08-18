//go:build !windows

package plugin

import "syscall"

// syscallZero 是探测进程是否存活用的空信号：不改变目标进程状态，
// 只让内核回答「这个 pid 还在不在」。
var syscallZero = syscall.Signal(0)
