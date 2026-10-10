package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/keystore"
	"github.com/linguo2625469/workbuddy2api-panel/internal/loginlock"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 网关侧（/v1、/status）与面板共用同一 api_key，枚举面也就是同一个：
// 失败达到阈值后必须 429 并带 Retry-After，锁定期内正确密钥同样拒绝。
func TestGatewayLoginLockout(t *testing.T) {
	lock := loginlock.New(loginlock.Policy{
		Enabled: true, MaxFails: 2, Window: time.Minute, LockDuration: time.Minute,
	})
	h := NewHandler(Config{
		Pool:         testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream:     upstream.New(),
		APIKey:       "secret",
		LoginLockout: lock,
	})

	call := func(path, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if rec := call("/v1/models", "bad"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("第 1 次失败应 401，got %d", rec.Code)
	}
	// 第 2 次失败即达阈值：429 + Retry-After + OpenAI 形态错误体。
	rec := call("/v1/models", "bad")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("达阈值应 429，got %d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 必须带 Retry-After")
	}
	if !strings.Contains(rec.Body.String(), "too_many_failed_attempts") {
		t.Errorf("错误体应带 too_many_failed_attempts 码，got %s", rec.Body.String())
	}
	// 锁定期内正确密钥也 429（管理面 /status 同理，同一记账）。
	if rec := call("/status", "secret"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("锁定期内正确密钥也应 429，got %d", rec.Code)
	}
	// /healthz 恒无鉴权：探活不能被登录失败锁定连坐，否则编排会摘掉实例。
	if rec := call("/healthz", ""); rec.Code != http.StatusOK {
		t.Fatalf("/healthz 不应受锁定影响，got %d", rec.Code)
	}
}

// 失效（禁用/过期）的分发密钥不计入失败次数：那是"有效凭据被停用"，持有者不是
// 枚举者；计入会让正常的密钥轮换把整个出口 IP 锁在门外。
func TestGatewayLockoutIgnoresDisabledKeys(t *testing.T) {
	keys, err := keystore.New(filepath.Join(t.TempDir(), "keys.json"))
	if err != nil {
		t.Fatalf("keystore.New: %v", err)
	}
	_, plain, err := keys.Create(keystore.CreateInput{Name: "off", Mode: keystore.ModeGlobal, Enabled: false})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	h := NewHandler(Config{
		Pool:         testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream:     upstream.New(),
		APIKey:       "secret",
		Keys:         keys,
		LoginLockout: loginlock.New(loginlock.Policy{Enabled: true, MaxFails: 2, Window: time.Minute, LockDuration: time.Minute}),
	})
	// 禁用密钥连打 3 次（超过阈值）：都是 401，且不累计失败 → 不锁定。
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest("GET", "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+plain)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次禁用密钥应 401，got %d", i+1, rec.Code)
		}
	}
	// 随后正确根密钥照常放行（若禁用密钥被计入，这里会是 429）。
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusTooManyRequests {
		t.Fatal("禁用密钥不应把来源锁上")
	}
}

// 未注入 Limiter（既有测试/裸用路径）时必须保持原样：错密钥恒 401，永不 429。
func TestGatewayWithoutLockoutKeepsLegacyBehavior(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: upstream.New(),
		APIKey:   "secret",
	})
	for i := 0; i < 6; i++ {
		req := httptest.NewRequest("GET", "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer bad")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("未启用锁定时第 %d 次失败应 401，got %d", i+1, rec.Code)
		}
	}
}
