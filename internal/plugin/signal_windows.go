//go:build windows

package plugin

import "os"

// sigTerm 在 Windows 上退化为 Kill。
//
// Windows 没有 SIGTERM 语义，向进程发送非 Kill 信号会直接失败。优雅退出的
// 机会已经由 Shutdown RPC 给过了，到这一步就直接终止。
func sigTerm() os.Signal { return os.Kill }
