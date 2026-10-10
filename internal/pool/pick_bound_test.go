package pool

import (
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
