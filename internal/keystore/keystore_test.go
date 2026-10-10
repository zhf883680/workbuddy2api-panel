package keystore

import (
	"strings"
	"testing"
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
