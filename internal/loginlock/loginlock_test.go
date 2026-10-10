package loginlock

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// testLimiter 固定时钟的 Limiter：now 可手动推进。
func testLimiter(p Policy) (*Limiter, *time.Time) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := New(p)
	l.now = func() time.Time { return now }
	return l, &now
}

func basePolicy() Policy {
	return Policy{Enabled: true, MaxFails: 3, Window: time.Minute, LockDuration: 5 * time.Minute}
}

func TestFailLocksAtThreshold(t *testing.T) {
	l, now := testLimiter(basePolicy())
	// 前两次失败只计数，未锁定。
	for i := 0; i < 2; i++ {
		if locked, _ := l.Fail("1.2.3.4"); locked {
			t.Fatalf("第 %d 次失败不应锁定", i+1)
		}
		if ok, _ := l.Allow("1.2.3.4"); !ok {
			t.Fatalf("第 %d 次失败后不应锁定", i+1)
		}
	}
	// 第三次失败触发锁定，并立即回锁定（而不是再放一次 401）。
	locked, retry := l.Fail("1.2.3.4")
	if !locked || retry != 5*time.Minute {
		t.Fatalf("第三次失败应锁定 5m，got locked=%v retry=%v", locked, retry)
	}
	if ok, retry := l.Allow("1.2.3.4"); ok || retry <= 0 {
		t.Fatalf("锁定期间应拒绝，got ok=%v retry=%v", ok, retry)
	}
	// 其它来源不受影响（按来源隔离）。
	if ok, _ := l.Allow("5.6.7.8"); !ok {
		t.Fatal("其它来源不应被连坐")
	}
	// 锁定期内的失败不延长锁定。
	*now = now.Add(time.Minute)
	locked, retry = l.Fail("1.2.3.4")
	if !locked || retry != 4*time.Minute {
		t.Fatalf("锁定期内失败应维持原锁，got locked=%v retry=%v", locked, retry)
	}
}

func TestLockExpiresAndRequiresFreshFailures(t *testing.T) {
	l, now := testLimiter(basePolicy())
	for i := 0; i < 3; i++ {
		l.Fail("1.2.3.4")
	}
	// 锁定到期即放行（自动解锁，无需人工干预）。
	*now = now.Add(5 * time.Minute)
	if ok, _ := l.Allow("1.2.3.4"); !ok {
		t.Fatal("锁定到期后应放行")
	}
	// 解锁后需要重新累计满阈值才再次锁定（不继承上一轮计数）。
	for i := 0; i < 2; i++ {
		if locked, _ := l.Fail("1.2.3.4"); locked {
			t.Fatalf("解锁后第 %d 次失败不应立即再锁", i+1)
		}
	}
	if locked, _ := l.Fail("1.2.3.4"); !locked {
		t.Fatal("解锁后重新累计满阈值应再次锁定")
	}
}

func TestWindowSlideResetsCounter(t *testing.T) {
	l, now := testLimiter(basePolicy())
	l.Fail("1.2.3.4")
	l.Fail("1.2.3.4")
	// 窗口滑过：前两次失败作废（否则"每天试两次"就永远锁不上，也不会被限制）。
	*now = now.Add(2 * time.Minute)
	if locked, _ := l.Fail("1.2.3.4"); locked {
		t.Fatal("窗口滑过后计数应从头开始，不应锁定")
	}
	if locked, _ := l.Fail("1.2.3.4"); locked {
		t.Fatal("窗口内第 2 次失败不应锁定")
	}
	if locked, _ := l.Fail("1.2.3.4"); !locked {
		t.Fatal("窗口内第 3 次失败应锁定")
	}
}

func TestSuccessDoesNotClearCounter(t *testing.T) {
	// 成功不清零：否则持有有效分发密钥的调用方能靠一次成功请求重置计数，
	// 枚举者可借"成功一次 + 失败四次"的循环绕过锁定。
	l, _ := testLimiter(basePolicy())
	l.Fail("1.2.3.4")
	l.Fail("1.2.3.4")
	l.Allow("1.2.3.4") // 放行（不代表成功请求会重置）
	if locked, _ := l.Fail("1.2.3.4"); !locked {
		t.Fatal("成功路径不应重置失败计数")
	}
}

func TestDisabledPolicyIsNoop(t *testing.T) {
	off := Policy{Enabled: false, MaxFails: 3, Window: time.Minute, LockDuration: time.Minute}
	l, _ := testLimiter(off)
	if l.Enabled() {
		t.Fatal("Enabled=false 时不应生效")
	}
	for i := 0; i < 10; i++ {
		if locked, _ := l.Fail("1.2.3.4"); locked {
			t.Fatal("关闭策略时永不应锁定")
		}
	}
	// 半配（缺窗口/锁时长）同样视为关闭：不能因为写了一半的配置把面板锁死。
	half := Policy{Enabled: true, MaxFails: 3}
	l2, _ := testLimiter(half)
	if l2.Enabled() {
		t.Fatal("参数不全时不应生效")
	}
	if locked, _ := l2.Fail("1.2.3.4"); locked {
		t.Fatal("参数不全时不应锁定")
	}
}

func TestSetPolicyTogglesAndClears(t *testing.T) {
	l, _ := testLimiter(Policy{})
	if l.Enabled() {
		t.Fatal("零值策略应为关闭")
	}
	l.SetPolicy(basePolicy())
	if !l.Enabled() {
		t.Fatal("SetPolicy 后应生效")
	}
	for i := 0; i < 3; i++ {
		l.Fail("1.2.3.4")
	}
	if ok, _ := l.Allow("1.2.3.4"); ok {
		t.Fatal("应处于锁定态")
	}
	// 关闭再打开：旧计数必须清干净，不能翻了旧账直接把来源锁上。
	l.SetPolicy(Policy{})
	l.SetPolicy(basePolicy())
	if ok, _ := l.Allow("1.2.3.4"); !ok {
		t.Fatal("重新启用后不应残留旧锁定")
	}
}

func TestNilLimiterSafe(t *testing.T) {
	var l *Limiter
	if l.Enabled() {
		t.Fatal("nil Limiter 应为关闭")
	}
	if ok, _ := l.Allow("x"); !ok {
		t.Fatal("nil Limiter 应放行")
	}
	if locked, _ := l.Fail("x"); locked {
		t.Fatal("nil Limiter 不应锁定")
	}
	l.SetPolicy(basePolicy()) // 不应 panic
}

func TestSecondCeil(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want int
	}{
		{0, 0},
		{-time.Second, 0},
		{time.Millisecond, 1},
		{999 * time.Millisecond, 1},
		{time.Second, 1},
		{1500 * time.Millisecond, 2},
		{10 * time.Minute, 600},
	}
	for _, c := range cases {
		if got := SecondCeil(c.in); got != c.want {
			t.Errorf("SecondCeil(%v)=%d want %d", c.in, got, c.want)
		}
	}
}

func TestClientKeyPrecedence(t *testing.T) {
	r := httptest.NewRequest("GET", "/panel/", nil)
	if got := ClientKey(r); got != "192.0.2.1" {
		t.Fatalf("无代理头应回落 RemoteAddr 主机部分，got %q", got)
	}
	r.Header.Set("X-Real-IP", "10.0.0.9")
	if got := ClientKey(r); got != "10.0.0.9" {
		t.Fatalf("应取 X-Real-IP，got %q", got)
	}
	r.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.9")
	if got := ClientKey(r); got != "203.0.113.7" {
		t.Fatalf("应取 XFF 首段，got %q", got)
	}
	// 直连（无端口形态的 RemoteAddr）也不能丢。
	var nilReq *http.Request
	if got := ClientKey(nilReq); got != "" {
		t.Fatalf("nil 请求应返回空键，got %q", got)
	}
}

func TestSweepKeepsMemoryBounded(t *testing.T) {
	l, now := testLimiter(basePolicy())
	for i := 0; i < sweepAfter*3; i++ {
		l.Fail("old-" + strconv.Itoa(i))
	}
	if len(l.entries) != sweepAfter*3 {
		t.Fatalf("窗口内条目应全部保留，got %d", len(l.entries))
	}
	// 窗口全部滑过：继续写入即触发扫除，过期条目（旧的 3 批）应被清掉，
	// 只剩本轮新写入的这一批。
	*now = now.Add(2 * time.Minute)
	for i := 0; i < sweepAfter; i++ {
		l.Fail("new-" + strconv.Itoa(i))
	}
	if got := len(l.entries); got > sweepAfter {
		t.Fatalf("过期条目未被扫除，仍剩 %d 条", got)
	}
}
