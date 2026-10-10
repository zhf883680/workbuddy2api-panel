package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/keystore"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

func newKeysTestPanel(t *testing.T) (*Panel, *keystore.Store) {
	t.Helper()
	ks, err := keystore.New(t.TempDir() + "/keys.json")
	if err != nil {
		t.Fatal(err)
	}
	return New(Config{Version: "test", APIKey: "secret", Keys: ks, Pool: pool.New("")}), ks
}

func postKeys(t *testing.T, p *Panel, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

func TestKeysAPICreateListRotateRemove(t *testing.T) {
	p, ks := newKeysTestPanel(t)

	rec := postKeys(t, p, "/panel/api/keys", `{"name":"小王","mode":"bound","uids":["u_a","u_a"],"enabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d (%s)", rec.Code, rec.Body.String())
	}
	var created struct {
		Key       keystore.View `json:"key"`
		Plaintext string        `json:"plaintext"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if created.Plaintext == "" {
		t.Fatal("创建响应必须返回一次明文")
	}
	if len(created.Key.UIDs) != 1 || created.Key.UIDs[0] != "u_a" {
		t.Fatalf("uids 未去重: %+v", created.Key.UIDs)
	}

	// 列表：不得出现明文，也不得出现摘要。
	get := httptest.NewRequest("GET", "/panel/api/keys", nil)
	get.Header.Set("Authorization", "Bearer secret")
	recG := httptest.NewRecorder()
	p.ServeHTTP(recG, get)
	if recG.Code != http.StatusOK {
		t.Fatalf("list status = %d", recG.Code)
	}
	body := recG.Body.String()
	if strings.Contains(body, created.Plaintext) || strings.Contains(body, "hash") {
		t.Fatalf("列表不得含明文或摘要: %s", body)
	}

	// 更新：改名 + 关停 + 改绑定。
	if rec := postKeys(t, p, "/panel/api/keys/"+created.Key.ID+"/update",
		`{"name":"小李","enabled":false,"uids":["u_a","u_b"]}`); rec.Code != http.StatusOK {
		t.Fatalf("update status = %d (%s)", rec.Code, rec.Body.String())
	}
	if list := ks.List(); list[0].Name != "小李" || list[0].Enabled || len(list[0].UIDs) != 2 {
		t.Fatalf("更新未生效: %+v", list[0])
	}

	// 轮换：旧明文立即失效（先把上一步关停的开关打开，避免测成"禁用"而非"轮换"）。
	if rec := postKeys(t, p, "/panel/api/keys/"+created.Key.ID+"/update", `{"enabled":true}`); rec.Code != http.StatusOK {
		t.Fatalf("re-enable status = %d (%s)", rec.Code, rec.Body.String())
	}
	recR := postKeys(t, p, "/panel/api/keys/"+created.Key.ID+"/rotate", ``)
	if recR.Code != http.StatusOK {
		t.Fatalf("rotate status = %d (%s)", recR.Code, recR.Body.String())
	}
	var rotated struct {
		Plaintext string `json:"plaintext"`
	}
	_ = json.Unmarshal(recR.Body.Bytes(), &rotated)
	if rotated.Plaintext == "" || rotated.Plaintext == created.Plaintext {
		t.Fatalf("轮换必须换明文: %+v", rotated)
	}
	if _, res := ks.Lookup(created.Plaintext); res != keystore.LookupMiss {
		t.Fatalf("旧明文应失效，得到 %v", res)
	}
	if _, res := ks.Lookup(rotated.Plaintext); res != keystore.LookupOK {
		t.Fatalf("新明文应可用，得到 %v", res)
	}

	// 删除 + 404。
	if rec := postKeys(t, p, "/panel/api/keys/"+created.Key.ID+"/remove", ``); rec.Code != http.StatusOK {
		t.Fatalf("remove status = %d", rec.Code)
	}
	if rec := postKeys(t, p, "/panel/api/keys/k_nope/remove", ``); rec.Code != http.StatusNotFound {
		t.Fatalf("删除不存在应 404，得到 %d", rec.Code)
	}
	if rec := postKeys(t, p, "/panel/api/keys/k_nope/rotate", ``); rec.Code != http.StatusNotFound {
		t.Fatalf("轮换不存在应 404，得到 %d", rec.Code)
	}
}

func TestKeysAPIValidation(t *testing.T) {
	p, ks := newKeysTestPanel(t)
	cases := []struct{ name, body string }{
		{"缺 name", `{"mode":"global"}`},
		{"缺 mode", `{"name":"x"}`},
		{"空名称", `{"name":"   ","mode":"global"}`},
		{"名称过长", `{"name":"` + strings.Repeat("字", 33) + `","mode":"global"}`},
		{"非法 mode", `{"name":"x","mode":"pool"}`},
		{"非法 uid", `{"name":"x","mode":"bound","uids":["../etc"]}`},
		{"过期时间在过去", `{"name":"x","mode":"global","expires_at":1}`},
		{"坏 JSON", `{"name":`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if rec := postKeys(t, p, "/panel/api/keys", tc.body); rec.Code != http.StatusBadRequest {
				t.Fatalf("应 400，得到 %d (%s)", rec.Code, rec.Body.String())
			}
		})
	}
	if list := ks.List(); len(list) != 0 {
		t.Fatalf("非法输入不得写入任何密钥: %+v", list)
	}
}

func TestKeysAPINilStoreAndRootOnly(t *testing.T) {
	// 未注入 keystore：接口 501（与既有可选依赖口径一致）。
	p := New(Config{Version: "test", APIKey: "secret", Pool: pool.New("")})
	if rec := postKeys(t, p, "/panel/api/keys", `{"name":"x","mode":"global"}`); rec.Code != http.StatusNotImplemented {
		t.Fatalf("nil store 应 501，得到 %d", rec.Code)
	}

	// 分发密钥不得访问面板管理接口：面板鉴权只认根密钥。
	p2, ks := newKeysTestPanel(t)
	_, distPlain, err := ks.Create(keystore.CreateInput{Name: "分发", Mode: keystore.ModeGlobal, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/panel/api/keys", nil)
	req.Header.Set("Authorization", "Bearer "+distPlain)
	rec := httptest.NewRecorder()
	p2.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("分发密钥访问面板应 401，得到 %d", rec.Code)
	}
}
