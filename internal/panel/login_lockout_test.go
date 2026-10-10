package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/loginlock"
)

// newLockoutPanel 构建带锁定策略的面板（api_key 已启用）。
func newLockoutPanel(policy loginlock.Policy) *Panel {
	return New(Config{
		Version:      "test",
		APIKey:       "good-key",
		LoginLockout: loginlock.New(policy),
	})
}

// getPanelAPI 以给定 token 打一个只读接口（/panel/api/logs 不依赖 Pool/Upstream）。
func getPanelAPI(p *Panel, token, remoteAddr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/panel/api/logs", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

// 面板鉴权失败达到阈值必须锁定：阈值内 401，达到即 429 + Retry-After；
// 锁定期内连正确密钥也拒（硬锁针对来源，不是凭据）。
func TestPanelLoginLockout(t *testing.T) {
	p := newLockoutPanel(loginlock.Policy{
		Enabled: true, MaxFails: 3, Window: time.Minute, LockDuration: 5 * time.Minute,
	})

	for i := 1; i <= 2; i++ {
		if rec := getPanelAPI(p, "bad-key", ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次失败应 401，got %d", i, rec.Code)
		}
	}
	// 第 3 次失败即达阈值：本次就回 429（前端立刻看到"已锁定"，而不是再报一次密钥错）。
	rec := getPanelAPI(p, "bad-key", "")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("达到阈值应 429，got %d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 必须带 Retry-After（客户端据此退避）")
	}
	if !strings.Contains(rec.Body.String(), "锁定") {
		t.Errorf("错误体应说明被锁定，got %s", rec.Body.String())
	}
	// 锁定期内：正确密钥同样 429（只放行正确凭据会让攻击者用一次成功把计数刷掉）。
	if rec := getPanelAPI(p, "good-key", ""); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("锁定期内正确密钥也应 429，got %d", rec.Code)
	}
	// 锁定只针对 API：面板页面本身无秘密（静态资源），照常可加载。
	recPage := httptest.NewRecorder()
	p.ServeHTTP(recPage, httptest.NewRequest("GET", "/panel/", nil))
	if recPage.Code != http.StatusOK {
		t.Fatalf("锁定不应影响面板页面加载，got %d", recPage.Code)
	}
}

// 锁定按来源隔离：一个来源被锁不影响其它来源登录。
func TestPanelLoginLockoutIsolatedBySource(t *testing.T) {
	p := newLockoutPanel(loginlock.Policy{
		Enabled: true, MaxFails: 2, Window: time.Minute, LockDuration: 10 * time.Minute,
	})
	// 第一个来源（httptest 默认 192.0.2.1）连打两次错密钥 → 锁定。
	getPanelAPI(p, "bad", "")
	if rec := getPanelAPI(p, "bad", ""); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("攻击来源应被锁定，got %d", rec.Code)
	}
	// 另一个来源（管理员）用正确密钥照常进入。
	if rec := getPanelAPI(p, "good-key", "198.51.100.9:4321"); rec.Code != http.StatusOK {
		t.Fatalf("其它来源不应被连坐，got %d", rec.Code)
	}
	// 代理头优先：X-Forwarded-For 首段是锁定维度（反代后取真实客户端）。
	req := httptest.NewRequest("GET", "/panel/api/logs", nil)
	req.Header.Set("Authorization", "Bearer good-key")
	req.Header.Set("X-Forwarded-For", "203.0.113.5, 10.0.0.1")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("XFF 来源应独立记账，got %d", rec.Code)
	}
}

// 未注入 Limiter（测试/裸用路径）时必须保持既有行为：只 401，永不锁定（零回归）。
func TestPanelWithoutLockoutKeepsLegacyBehavior(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "good-key"})
	for i := 0; i < 10; i++ {
		if rec := getPanelAPI(p, "bad", ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("未启用锁定时第 %d 次失败应 401，got %d", i+1, rec.Code)
		}
	}
	if rec := getPanelAPI(p, "good-key", ""); rec.Code != http.StatusOK {
		t.Fatalf("未启用锁定时正确密钥应放行，got %d", rec.Code)
	}
}

// 未启用鉴权（api_key 空）时不记账：没有任何凭据可试错，不该把调用方锁在门外。
func TestPanelLockoutSkippedWhenAuthDisabled(t *testing.T) {
	p := New(Config{
		Version:      "test",
		LoginLockout: loginlock.New(loginlock.Policy{Enabled: true, MaxFails: 1, Window: time.Minute, LockDuration: time.Minute}),
	})
	for i := 0; i < 5; i++ {
		if rec := getPanelAPI(p, "", ""); rec.Code != http.StatusOK {
			t.Fatalf("未启用鉴权时第 %d 次请求应放行，got %d", i+1, rec.Code)
		}
	}
}
