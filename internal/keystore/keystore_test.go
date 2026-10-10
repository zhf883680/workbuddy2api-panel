package keystore

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestCreateThenLookup(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir + "/keys.json")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	k, plain, err := s.Create(CreateInput{Name: "张三", Mode: ModeBound, UIDs: []string{"u_b", "u_a", "u_a"}, Enabled: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasPrefix(plain, "sk-") || len(plain) < 20 {
		t.Fatalf("plaintext %q 形态不对", plain)
	}
	if k.Name != "张三" || k.Mode != ModeBound {
		t.Fatalf("view = %+v", k)
	}
	if len(k.UIDs) != 2 || k.UIDs[0] != "u_a" || k.UIDs[1] != "u_b" {
		t.Fatalf("uids 未去重排序: %+v", k.UIDs)
	}
	if !strings.HasPrefix(plain, strings.TrimSuffix(k.Prefix, "…")) {
		t.Fatalf("prefix %q 与明文 %q 不匹配", k.Prefix, plain)
	}

	key, res := s.Lookup(plain)
	if res != LookupOK || key.ID != k.ID {
		t.Fatalf("Lookup(plain) = %v/%v", key, res)
	}
	if _, res := s.Lookup("sk-nope"); res != LookupMiss {
		t.Fatalf("未知密钥应 Miss，得到 %v", res)
	}
}

func TestCreateGlobalClearsUIDs(t *testing.T) {
	s, _ := New(t.TempDir() + "/keys.json")
	k, _, err := s.Create(CreateInput{Name: "全局", Mode: ModeGlobal, UIDs: []string{"u_a"}, Enabled: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(k.UIDs) != 0 {
		t.Fatalf("global 模式必须清空 uids，得到 %+v", k.UIDs)
	}
}

func TestLookupDisabledAndExpired(t *testing.T) {
	s, _ := New(t.TempDir() + "/keys.json")
	_, plain, _ := s.Create(CreateInput{Name: "停用", Mode: ModeGlobal, Enabled: false})
	if _, res := s.Lookup(plain); res != LookupDisabled {
		t.Fatalf("禁用密钥应 LookupDisabled，得到 %v", res)
	}
	_, plain2, _ := s.Create(CreateInput{Name: "过期", Mode: ModeGlobal, Enabled: true, ExpiresAt: time.Now().Add(-time.Minute).Unix()})
	if _, res := s.Lookup(plain2); res != LookupExpired {
		t.Fatalf("过期密钥应 LookupExpired，得到 %v", res)
	}
}

func TestUpdateRotateRemove(t *testing.T) {
	s, _ := New(t.TempDir() + "/keys.json")
	created, oldPlain, _ := s.Create(CreateInput{Name: "A", Mode: ModeBound, UIDs: []string{"u_a"}, Enabled: true})

	name := "B"
	exp := time.Now().Add(time.Hour).Unix()
	upd, err := s.Update(created.ID, UpdateInput{Name: &name, Mode: strPtr(ModeGlobal), ExpiresAt: &exp})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if upd.Name != "B" || upd.Mode != ModeGlobal || len(upd.UIDs) != 0 || upd.ExpiresAt != exp {
		t.Fatalf("Update 结果 = %+v", upd)
	}

	rot, newPlain, err := s.Rotate(created.ID)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if newPlain == oldPlain || !strings.HasPrefix(newPlain, strings.TrimSuffix(rot.Prefix, "…")) {
		t.Fatalf("轮换必须换明文且 prefix 同步更新")
	}
	if _, res := s.Lookup(oldPlain); res != LookupMiss {
		t.Fatalf("旧明文轮换后应 Miss，得到 %v", res)
	}
	if _, res := s.Lookup(newPlain); res != LookupOK {
		t.Fatalf("新明文应可用，得到 %v", res)
	}
	if rot.ID != created.ID || rot.Name != "B" {
		t.Fatalf("轮换不应改动 ID/Name: %+v", rot)
	}

	if err := s.Remove(created.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, res := s.Lookup(newPlain); res != LookupMiss {
		t.Fatalf("删除后应 Miss，得到 %v", res)
	}
	if err := s.Remove(created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("重复删除应 ErrNotFound，得到 %v", err)
	}
	if _, err := s.Update("k_nope", UpdateInput{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("更新不存在的 ID 应 ErrNotFound，得到 %v", err)
	}
}

func strPtr(s string) *string { return &s }

func TestPersistAndReload(t *testing.T) {
	path := t.TempDir() + "/keys.json"
	s, _ := New(path)
	k, plain, _ := s.Create(CreateInput{Name: "持久", Mode: ModeBound, UIDs: []string{"u_a"}, Enabled: true})
	s.NoteUsage(k.ID, false, 10, 20)
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("权限应 0600，得到 %v", info.Mode().Perm())
	}

	s2, err := New(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, res := s2.Lookup(plain); res != LookupOK {
		t.Fatalf("重载后应命中，得到 %v", res)
	}
	list := s2.List()
	if len(list) != 1 || list[0].Usage.Req != 1 || list[0].Usage.Err != 1 ||
		list[0].Usage.PromptTokens != 10 || list[0].Usage.CompletionTokens != 20 {
		t.Fatalf("统计未持久化: %+v", list)
	}
	if list[0].LastUsedAt == 0 {
		t.Fatalf("LastUsedAt 未更新")
	}
}

func TestNoteUsageDebouncedFlush(t *testing.T) {
	path := t.TempDir() + "/keys.json"
	s, _ := New(path)
	k, _, _ := s.Create(CreateInput{Name: "防抖", Mode: ModeGlobal, Enabled: true})
	s.flushEvery = 20 * time.Millisecond
	s.Start()
	defer s.Stop()
	s.NoteUsage(k.ID, true, 1, 2) // 只标脏，不立刻落盘
	time.Sleep(120 * time.Millisecond)
	s2, _ := New(path)
	if got := s2.List()[0].Usage.Req; got != 1 {
		t.Fatalf("防抖刷盘后 req = %d, want 1", got)
	}
}
