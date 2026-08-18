//go:build windows

package plugin

import "os"

// syscallZero 在 Windows 上没有等价的空信号；用 Kill 会真的杀掉进程，
// 因此存活探测在该平台上意义有限，这里给一个占位值。
var syscallZero os.Signal = os.Interrupt
