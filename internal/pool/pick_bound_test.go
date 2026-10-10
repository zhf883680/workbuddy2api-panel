package pool

import (
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// pickBoundTestPool 建一个含 A/B/C 三个健康账号的池。
func pickBoundTestPool(t *testing.T) *Pool {
	t.Helper()
	p := New("")
	for _, uid := range []string{"u_a", "u_b", "u_c"} {
		p.Add(&auth.Auth{UID: uid, Nickname: uid})
	}
	return p
}

func TestPickBoundOnlyWithinAllowed(t *testing.T) {
	p := pickBoundTestPool(t)
	allowed := map[string]bool{"u_b": true}
	for i := 0; i < 20; i++ {
		a := p.PickExcludingForRealmBound(nil, "", "", allowed)
		if a == nil {
			t.Fatalf("第 %d 次选号返回 nil", i)
		}
		if a.UID != "u_b" {
			t.Fatalf("bound 选号越界: %s", a.UID)
		}
	}
}

func TestPickBoundEmptyAllowedReturnsNil(t *testing.T) {
	p := pickBoundTestPool(t)
	if a := p.PickExcludingForRealmBound(nil, "", "", map[string]bool{}); a != nil {
		t.Fatalf("空绑定集合必须返回 nil，得到 %s", a.UID)
	}
}

// TestPickBoundFallbackStaysInsideAllowed 严格模式的核心保证：全冷却兜底
// （pickEarliestExpiryLocked）只能在绑定集合内挑，绝不外溢到健康的非绑定账号。
func TestPickBoundFallbackStaysInsideAllowed(t *testing.T) {
	p := pickBoundTestPool(t)
	// u_a 软冷却（兜底可参与），u_b/u_c 健康 —— 没有约束时兜底/正常选号都会挑到它们。
	p.Cooldown("u_a", CoolSoft, time.Minute, "test")
	allowed := map[string]bool{"u_a": true}
	for i := 0; i < 5; i++ {
		got := p.PickExcludingForRealmBound(nil, "", "", allowed)
		if got == nil {
			t.Fatal("软冷却的绑定号应被兜底选中")
		}
		if got.UID != "u_a" {
			t.Fatalf("兜底越界: %s", got.UID)
		}
	}

	// 绑定集合只含不存在的 uid：必须选不出号（不允许回落全局池）。
	if a := p.PickExcludingForRealmBound(nil, "", "", map[string]bool{"u_ghost": true}); a != nil {
		t.Fatalf("不存在的绑定 uid 不该选到任何号，得到 %s", a.UID)
	}
	// 硬冷却（余额耗尽）的绑定号不参与兜底：同样必须 nil。
	p.Cooldown("u_b", CoolHard, time.Hour, "test")
	if a := p.PickExcludingForRealmBound(nil, "", "", map[string]bool{"u_b": true}); a != nil {
		t.Fatalf("硬冷却号不该被兜底选中，得到 %s", a.UID)
	}
}

func TestExplainBound(t *testing.T) {
	p := pickBoundTestPool(t)
	p.Disable("u_a", "test")
	p.Pause("u_c")
	got := p.ExplainBound([]string{"u_a", "u_b", "u_c", "u_ghost"}, "cn", "")
	byUID := map[string]UnavailableReason{}
	for _, r := range got {
		byUID[r.UID] = r
	}
	if byUID["u_a"].Code != "disabled" {
		t.Fatalf("u_a = %+v", byUID["u_a"])
	}
	if byUID["u_b"].Code != "ok" {
		t.Fatalf("u_b 应可用: %+v", byUID["u_b"])
	}
	if byUID["u_c"].Code != "paused" {
		t.Fatalf("u_c = %+v", byUID["u_c"])
	}
	if byUID["u_ghost"].Code != "not_found" {
		t.Fatalf("u_ghost = %+v", byUID["u_ghost"])
	}
}

func TestExplainBoundRealmMismatch(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u_g", Nickname: "g", Domain: "workbuddy.ai"})
	got := p.ExplainBound([]string{"u_g"}, "cn", "")
	if len(got) != 1 || got[0].Code != "realm_mismatch" {
		t.Fatalf("跨域账号应判 realm_mismatch，得到 %+v", got)
	}
}

func TestSummarizeUnavailable(t *testing.T) {
	got := SummarizeUnavailable([]UnavailableReason{
		{UID: "a", Code: "cooling", Until: time.Now().Add(30 * time.Minute)},
		{UID: "b", Code: "in_flight_full"},
		{UID: "c", Code: "ok"},
	})
	if !strings.Contains(got, "冷却中") || !strings.Contains(got, "在途占满") {
		t.Fatalf("摘要 = %q", got)
	}
	if SummarizeUnavailable([]UnavailableReason{{UID: "a", Code: "ok"}}) != "" {
		t.Fatal("全部可用应返回空串")
	}
	if SummarizeUnavailable(nil) != "" {
		t.Fatal("空输入应返回空串")
	}
}
