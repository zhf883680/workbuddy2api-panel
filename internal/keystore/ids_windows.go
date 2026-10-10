//go:build windows

package keystore

// processIDs Windows 无 uid/gid 概念，返回 ok=false（错误信息里不展示身份）。
func processIDs() (uid, gid int, ok bool) { return 0, 0, false }
