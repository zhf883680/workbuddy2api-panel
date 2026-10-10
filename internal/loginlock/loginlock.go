// Package loginlock 鉴权失败处理：按来源（客户端 IP）累计失败次数，超过阈值即
// 临时锁定，锁定期内一律拒绝——连同正确密钥一起拒。
//
// 背景：面板与网关共用同一个 api_key（Bearer），而密钥是唯一凭据。此前鉴权失败
// 只回 401，既无次数限制也无冷却：拿到了线上地址就能无成本地一直枚举密钥，一旦
// 命中即拿到全部账号的运维面（等保「登录失败处理」项缺失）。本包把「失败计数 →
// 锁定 → 自动解锁」这段状态机单独成包，供 panel 与 server 共用同一实例、同一口径。
//
// 语义（滑动窗口 + 硬锁）：
//   - 同一来源在 Window 内累计 MaxFails 次鉴权失败即锁定 LockDuration；
//   - 锁定期内的请求一律拒绝（含携带正确密钥的请求）：锁定针对"来源"，与
//     「账号被锁定」同义——只放行正确凭据会让攻击者用一次成功请求把计数刷掉；
//   - 只有失败累加计数，成功不清零：否则任何持有有效分发密钥的调用方都能靠
//     一次成功请求重置自己来源上的计数，锁定形同虚设；
//   - 锁定期内到达的失败**不再累加**，锁不因持续探测而无限延长（一轮一锁）。
//
// 状态全在进程内存：重启即清空（也是运维的兜底解锁手段）。容量有界——条目按
// 来源键常驻但会被定期扫除（见 sweepLocked），活跃失败来源数即上界。
package loginlock

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// Policy 锁定策略（进程内可热改：面板在线改配置即通过 SetPolicy 换新）。
type Policy struct {
	Enabled      bool          // false = 关闭（鉴权失败照旧只回 401，行为与既有版本一致）
	MaxFails     int           // 窗口内允许的失败次数，达到即锁定
	Window       time.Duration // 失败计数窗口
	LockDuration time.Duration // 锁定持续时长
}

// Active 报告策略是否可执行。
// 任一参数缺失（含 Enabled 未置位）即视为关闭——半配状态不锁，避免把面板误锁死。
func (p Policy) Active() bool {
	return p.Enabled && p.MaxFails > 0 && p.Window > 0 && p.LockDuration > 0
}

// Limiter 按来源键维护失败计数与锁定截止时间（并发安全）。
type Limiter struct {
	mu      sync.Mutex
	now     func() time.Time // 可注入时钟（测试用）
	policy  Policy
	entries map[string]*entry
	inserts int // 距上次扫除的新建条目数（内存有界，见 sweepLocked）
}

// entry 单个来源的计数状态。
type entry struct {
	fails       int       // 当前窗口内的失败次数
	windowStart time.Time // 当前计数窗口起点
	lockedUntil time.Time // 零值 = 未锁定
}

// sweepAfter 每新建这么多条目触发一次过期扫除（摊薄到写入路径，无后台 goroutine）。
const sweepAfter = 256

// New 以初始策略构建 Limiter。
func New(p Policy) *Limiter {
	return &Limiter{now: time.Now, policy: p, entries: map[string]*entry{}}
}

// SetPolicy 整体替换策略（在线改配置热生效）。关闭策略时顺带清空计数，
// 避免"关掉再打开"时翻出很久以前的旧计数把来源直接锁上。
func (l *Limiter) SetPolicy(p Policy) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.policy = p
	if !p.Active() {
		l.entries = map[string]*entry{}
		l.inserts = 0
	}
}

// Enabled 报告当前策略是否生效（未启用时调用方可跳过整个记账路径）。
func (l *Limiter) Enabled() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.policy.Active()
}

// Allow 查询来源当前是否放行；被锁定返回 false 与剩余锁定时间。
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.policy.Active() {
		return true, 0
	}
	e := l.entries[key]
	if e == nil {
		return true, 0
	}
	now := l.now()
	if retry := e.remainingLock(now); retry > 0 {
		return false, retry
	}
	// 锁已过期 / 计数窗口已滑过：条目已无信息量，就地删除（顺手清理内存）。
	if e.windowStart.IsZero() || now.Sub(e.windowStart) > l.policy.Window {
		delete(l.entries, key)
	}
	return true, 0
}

// Fail 记录一次鉴权失败；达到阈值返回 locked=true 与锁定剩余时长。
// 锁定期内的失败不累加、不延长锁定（一轮一锁）。
func (l *Limiter) Fail(key string) (bool, time.Duration) {
	if l == nil {
		return false, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.policy.Active() {
		return false, 0
	}
	now := l.now()
	e := l.entries[key]
	if e == nil {
		l.inserts++
		if l.inserts >= sweepAfter {
			l.sweepLocked(now)
		}
		e = &entry{windowStart: now}
		l.entries[key] = e
	}
	if retry := e.remainingLock(now); retry > 0 {
		return true, retry
	}
	// 窗口滑过 → 计数从头开始（上一轮的失败不再累计）。
	if e.windowStart.IsZero() || now.Sub(e.windowStart) > l.policy.Window {
		e.fails = 0
		e.windowStart = now
	}
	e.fails++
	if e.fails >= l.policy.MaxFails {
		e.fails = 0
		e.windowStart = time.Time{} // 解锁后重新计数，不继承本轮
		e.lockedUntil = now.Add(l.policy.LockDuration)
		return true, l.policy.LockDuration
	}
	return false, 0
}

// LockedUntil 返回来源的锁定截止时刻（观测/测试用；未锁定返回零值与 false）。
func (l *Limiter) LockedUntil(key string) (time.Time, bool) {
	if l == nil {
		return time.Time{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[key]
	if e == nil {
		return time.Time{}, false
	}
	if e.lockedUntil.After(l.now()) {
		return e.lockedUntil, true
	}
	return time.Time{}, false
}

// sweepLocked 删除既未锁定又已过计数窗口的条目（调用方须持锁）。
func (l *Limiter) sweepLocked(now time.Time) {
	l.inserts = 0
	for k, e := range l.entries {
		if e.remainingLock(now) > 0 {
			continue
		}
		if e.windowStart.IsZero() || now.Sub(e.windowStart) > l.policy.Window {
			delete(l.entries, k)
		}
	}
}

// remainingLock 返回锁定剩余时长（未锁定为 0）。
func (e *entry) remainingLock(now time.Time) time.Duration {
	if e.lockedUntil.After(now) {
		return e.lockedUntil.Sub(now)
	}
	return 0
}

// SecondCeil 把剩余时长向上取整成秒（Retry-After 头与文案用；至少 1 秒，
// 避免显示「0 秒后重试」而客户端立刻又撞上锁）。
func SecondCeil(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	sec := int((d + time.Second - 1) / time.Second)
	if sec < 1 {
		sec = 1
	}
	return sec
}

// ClientKey 提取锁定维度：X-Forwarded-For 首段 → X-Real-IP → RemoteAddr 主机部分。
//
// 与请求日志的来源口径完全一致（server.clientIPForLog 同一实现）：代理后取真实
// 客户端，直连时回落对端地址。注意代理头可被伪造——锁定是纵深防御的一环，公网
// 部署仍应把面板置于反向代理之后并由代理限制请求速率（README 部署提示）。
func ClientKey(r *http.Request) string {
	if ip := upstream.ExtractClientIP(r); ip != "" {
		return ip
	}
	if r == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return host
}
