package keystore

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 写盘失败时必须给出可执行的诊断信息（绝对路径 / 目录可写性 / 处置建议），
// 而不是一句 "write tmp: permission denied" —— 后者在远端部署里无法排查。
func TestSaveFailureIsActionableAndRollsBack(t *testing.T) {
	dir := t.TempDir()
	s, err := New(filepath.Join(dir, "keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Create(CreateInput{Name: "ok", Mode: ModeGlobal, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	before := len(s.List())

	if err := os.Chmod(dir, 0o555); err != nil { // 模拟宿主机挂载目录属主 ≠ 容器 uid
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o755)

	_, _, err = s.Create(CreateInput{Name: "fail", Mode: ModeGlobal, Enabled: true})
	if err == nil {
		t.Skip("以 root 运行，权限检查不生效")
	}
	msg := err.Error()
	for _, want := range []string{"permission denied", dir, "可写", "chown"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误信息缺少 %q：\n%s", want, msg)
		}
	}
	// 落盘失败必须回滚内存状态：否则面板报错、密钥却在内存里生效，重启后又消失。
	if got := len(s.List()); got != before {
		t.Fatalf("落盘失败后内存未回滚：List()=%d, want %d", got, before)
	}
}

// 遗留的只读 keys.json.tmp（例如曾被 root 创建）不得把后续写入卡死。
func TestStaleReadOnlyTmpDoesNotWedge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.json")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Create(CreateInput{Name: "a", Mode: ModeGlobal, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	stale := path + ".tmp"
	if err := os.WriteFile(stale, []byte("stale"), 0o444); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Create(CreateInput{Name: "b", Mode: ModeGlobal, Enabled: true}); err != nil {
		t.Fatalf("遗留只读 tmp 不该阻断写入: %v", err)
	}
	if got := len(s.List()); got != 2 {
		t.Fatalf("List()=%d, want 2", got)
	}
}

// 防抖刷盘失败必须落日志（此前是 `_ = s.Save()` 静默吞掉 → 统计悄悄丢）。
func TestFlushFailureIsLogged(t *testing.T) {
	dir := t.TempDir()
	s, err := New(filepath.Join(dir, "keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	k, _, err := s.Create(CreateInput{Name: "x", Mode: ModeGlobal, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o755)

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	s.flushEvery = 20 * time.Millisecond
	s.Start()
	s.NoteUsage(k.ID, true, 1, 1)
	time.Sleep(150 * time.Millisecond)
	s.Stop()

	logged := buf.String()
	if !strings.Contains(logged, "[keystore]") {
		t.Skip("以 root 运行，权限检查不生效")
	}
	if !strings.Contains(logged, "落盘失败") || !strings.Contains(logged, "chown") {
		t.Fatalf("刷盘失败日志缺少诊断信息：%q", logged)
	}
	// 去重：周期性刷盘失败不能每 5s 刷一条（错误文案必须稳定，不能含随机临时文件名）。
	if n := strings.Count(logged, "[keystore]"); n != 1 {
		t.Fatalf("同一条刷盘错误应只记 1 次，实际 %d 次：%q", n, logged)
	}
}

// 路径在 New 时解析为绝对路径：错误信息里不该出现相对路径（远端部署下无从定位）。
func TestNewResolvesAbsolutePath(t *testing.T) {
	s, err := New("data/keys.json")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(s.path) {
		t.Fatalf("path 未解析为绝对路径: %q", s.path)
	}
}
