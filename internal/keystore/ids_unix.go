//go:build !windows

package keystore

import "os"

// processIDs 返回当前进程的 uid/gid（Windows 无此概念，见 ids_windows.go）。
// 用途：写盘失败时在错误信息里给出身份，便于对照挂载目录的属主排查。
func processIDs() (uid, gid int, ok bool) {
	return os.Getuid(), os.Getgid(), true
}
