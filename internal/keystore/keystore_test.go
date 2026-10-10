package keystore

import (
	"errors"
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
