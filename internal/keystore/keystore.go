// Package keystore 网关分发密钥的存储与校验。
//
// 定位：密钥是**运行期数据**（面板随时增删改），与 config.json（人改的配置）分离，
// 落在 keys.json（默认与 state_file 同目录）。内存侧是不可变快照 + sha256 摘要索引的
// O(1) 查表；写路径串行化，增删改立即原子落盘，统计计数走防抖。
package keystore

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// 模式枚举。
const (
	ModeGlobal = "global" // 走全局池选号
	ModeBound  = "bound"  // 只用 UIDs 绑定的账号，空列表 = 一律不可用
)

// CreateInput 创建密钥的入参（校验在调用方 panel 完成，本包只做规范化）。
type CreateInput struct {
	Name      string
	Mode      string
	UIDs      []string
	ExpiresAt int64 // Unix 秒；0 = 永不过期
	Enabled   bool
}

// Usage 一把密钥的累计用量。口径与 internal/usage 一致：每次上游尝试各记一次（含重试）。
type Usage struct {
	Req              int64 `json:"req"`
	Err              int64 `json:"err"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

// Key 一把分发密钥的完整记录（含摘要，仅进程内与落盘使用）。
type Key struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Hash       string   `json:"hash"`   // sha256 hex，永不外泄
	Prefix     string   `json:"prefix"` // 展示用前缀，形如 sk-ab12cd34…
	Mode       string   `json:"mode"`
	UIDs       []string `json:"uids,omitempty"`
	Enabled    bool     `json:"enabled"`
	ExpiresAt  int64    `json:"expires_at,omitempty"`
	CreatedAt  int64    `json:"created_at"`
	LastUsedAt int64    `json:"last_used_at,omitempty"`
	Usage      Usage    `json:"usage"`
}

// View 面向面板/日志的只读投影：**不含 Hash**，任何 HTTP 响应只能用它。
type View struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Prefix     string   `json:"prefix"`
	Mode       string   `json:"mode"`
	UIDs       []string `json:"uids"`
	Enabled    bool     `json:"enabled"`
	ExpiresAt  int64    `json:"expires_at"`
	CreatedAt  int64    `json:"created_at"`
	LastUsedAt int64    `json:"last_used_at"`
	Usage      Usage    `json:"usage"`
}

func (k *Key) view() View {
	uids := k.UIDs
	if uids == nil {
		uids = []string{}
	}
	return View{
		ID: k.ID, Name: k.Name, Prefix: k.Prefix, Mode: k.Mode, UIDs: uids,
		Enabled: k.Enabled, ExpiresAt: k.ExpiresAt, CreatedAt: k.CreatedAt,
		LastUsedAt: k.LastUsedAt, Usage: k.Usage,
	}
}

// LookupResult 一次查表的结果。
type LookupResult int

const (
	LookupMiss     LookupResult = iota // 没这把密钥
	LookupOK                           // 命中且可用
	LookupDisabled                     // 命中但被禁用
	LookupExpired                      // 命中但已过期
)

// Snapshot 一次读取的不可变视图（byHash 用于鉴权，list 按创建时间升序供面板展示）。
type Snapshot struct {
	byHash map[string]*Key
	list   []*Key
}

// ErrNotFound 目标密钥 ID 不存在。
var ErrNotFound = errors.New("keystore: key not found")

// Store 密钥存储。零值不可用，必须经 New 构建。
type Store struct {
	path string
	now  func() time.Time

	mu   sync.Mutex // 写路径串行化（CRUD 与 flush）
	snap atomic.Pointer[Snapshot]

	dirty      atomic.Bool
	flushEvery time.Duration
	startOnce  sync.Once
	stopOnce   sync.Once
	stopCh     chan struct{}
	doneCh     chan struct{}
}

// New 构建存储并从 path 读取已有密钥（文件不存在 = 空表，不是错误）。
func New(path string) (*Store, error) {
	s := &Store{
		path:       path,
		now:        time.Now,
		flushEvery: 5 * time.Second,
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
	}
	s.snap.Store(&Snapshot{byHash: map[string]*Key{}, list: nil})
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// HashToken 返回明文的存储摘要（持久化与查表用；不可逆，永不外泄）。
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// newPlaintext 生成明文密钥：sk- + 24 字节随机（RawURL 无填充）。
func newPlaintext() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("keystore: gen key: %w", err)
	}
	return "sk-" + base64.RawURLEncoding.EncodeToString(buf), nil
}

// newID 生成密钥 ID：k + 6 字节随机 hex（12 位）。
func newID() (string, error) {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("keystore: gen id: %w", err)
	}
	return "k" + hex.EncodeToString(buf), nil
}

// normalize 规范 mode/uids：未知 mode 一律 global；global 清空 uids；bound 去重升序。
func normalize(mode string, uids []string) (string, []string) {
	if mode != ModeBound {
		mode = ModeGlobal
	}
	if mode == ModeGlobal {
		return mode, nil
	}
	set := make(map[string]struct{}, len(uids))
	out := make([]string, 0, len(uids))
	for _, u := range uids {
		if u == "" {
			continue
		}
		if _, dup := set[u]; dup {
			continue
		}
		set[u] = struct{}{}
		out = append(out, u)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return mode, nil
	}
	return mode, out
}

// Create 新签发一把密钥，返回只读投影与**明文（仅此一次）**。
func (s *Store) Create(in CreateInput) (View, string, error) {
	plain, err := newPlaintext()
	if err != nil {
		return View{}, "", err
	}
	id, err := newID()
	if err != nil {
		return View{}, "", err
	}
	mode, uids := normalize(in.Mode, in.UIDs)
	k := &Key{
		ID: id, Name: in.Name, Hash: HashToken(plain), Prefix: plain[:11] + "…",
		Mode: mode, UIDs: uids, Enabled: in.Enabled, ExpiresAt: in.ExpiresAt,
		CreatedAt: s.now().Unix(),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.snap.Load()
	byHash := make(map[string]*Key, len(cur.byHash)+1)
	for h, v := range cur.byHash {
		byHash[h] = v
	}
	byHash[k.Hash] = k
	list := append(append([]*Key{}, cur.list...), k)
	s.snap.Store(&Snapshot{byHash: byHash, list: list})
	if err := s.saveLocked(); err != nil {
		return View{}, "", err
	}
	return k.view(), plain, nil
}

// Lookup 按明文查表并判定可用性。token 为空恒 Miss。
func (s *Store) Lookup(token string) (*Key, LookupResult) {
	if token == "" {
		return nil, LookupMiss
	}
	snap := s.snap.Load()
	if snap == nil {
		return nil, LookupMiss
	}
	k, ok := snap.byHash[HashToken(token)]
	if !ok {
		return nil, LookupMiss
	}
	if !k.Enabled {
		return k, LookupDisabled
	}
	if k.ExpiresAt > 0 && s.now().Unix() >= k.ExpiresAt {
		return k, LookupExpired
	}
	return k, LookupOK
}

// List 返回全部密钥的只读投影（创建时间升序，与内部 list 同序）。
func (s *Store) List() []View {
	snap := s.snap.Load()
	if snap == nil {
		return nil
	}
	out := make([]View, 0, len(snap.list))
	for _, k := range snap.list {
		out = append(out, k.view())
	}
	return out
}

// UpdateInput 局部更新：nil 字段 = 保持不变（指针语义区分「显式传 false/空」与「不传」）。
type UpdateInput struct {
	Name      *string
	Mode      *string
	UIDs      *[]string
	ExpiresAt *int64
	Enabled   *bool
}

// Update 局部更新一把密钥，返回更新后的投影。不存在返回 ErrNotFound。
func (s *Store) Update(id string, in UpdateInput) (View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.snap.Load()
	old, ok := findLocked(cur, id)
	if !ok {
		return View{}, ErrNotFound
	}
	next := *old // 值拷贝：只改字段，不污染旧快照（旧快照可能正被别人读）
	if in.Name != nil {
		next.Name = *in.Name
	}
	if in.Mode != nil {
		next.Mode, next.UIDs = normalize(*in.Mode, next.UIDs)
	}
	if in.UIDs != nil {
		next.Mode, next.UIDs = normalize(next.Mode, *in.UIDs)
	}
	if in.ExpiresAt != nil {
		next.ExpiresAt = *in.ExpiresAt
	}
	if in.Enabled != nil {
		next.Enabled = *in.Enabled
	}
	s.replaceLocked(old, &next)
	if err := s.saveLocked(); err != nil {
		return View{}, err
	}
	return next.view(), nil
}

// Rotate 轮换明文：换 Hash/Prefix，保留 ID/Name/绑定/开关/过期/统计。
func (s *Store) Rotate(id string) (View, string, error) {
	plain, err := newPlaintext()
	if err != nil {
		return View{}, "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.snap.Load()
	old, ok := findLocked(cur, id)
	if !ok {
		return View{}, "", ErrNotFound
	}
	next := *old
	next.Hash = HashToken(plain)
	next.Prefix = plain[:11] + "…"
	s.replaceLocked(old, &next)
	if err := s.saveLocked(); err != nil {
		return View{}, "", err
	}
	return next.view(), plain, nil
}

// Remove 删除一把密钥。
func (s *Store) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.snap.Load()
	if _, ok := findLocked(cur, id); !ok {
		return ErrNotFound
	}
	byHash := make(map[string]*Key, len(cur.byHash))
	for h, v := range cur.byHash {
		if v.ID == id {
			continue
		}
		byHash[h] = v
	}
	list := make([]*Key, 0, len(cur.list))
	for _, v := range cur.list {
		if v.ID == id {
			continue
		}
		list = append(list, v)
	}
	s.snap.Store(&Snapshot{byHash: byHash, list: list})
	return s.saveLocked()
}

// findLocked 按 ID 在快照里查（密钥数量有界，线性扫描足够且省一层索引维护）。
func findLocked(snap *Snapshot, id string) (*Key, bool) {
	if snap == nil {
		return nil, false
	}
	for _, k := range snap.list {
		if k.ID == id {
			return k, true
		}
	}
	return nil, false
}

// replaceLocked 用 next 替换 old（按 ID 同时更新 byHash 与 list），旧明文摘要同步移除。
func (s *Store) replaceLocked(old, next *Key) {
	cur := s.snap.Load()
	byHash := make(map[string]*Key, len(cur.byHash))
	for h, v := range cur.byHash {
		if v.ID == old.ID {
			continue
		}
		byHash[h] = v
	}
	byHash[next.Hash] = next
	list := make([]*Key, 0, len(cur.list))
	for _, v := range cur.list {
		if v.ID == old.ID {
			list = append(list, next)
			continue
		}
		list = append(list, v)
	}
	s.snap.Store(&Snapshot{byHash: byHash, list: list})
}

// ensureDir 确保父目录存在。
func ensureDir(path string) error {
	dir := filepath.Dir(path)
	if dir == "" || dir == "." {
		return nil
	}
	return os.MkdirAll(dir, 0o755)
}

// load 读取 keys.json；文件不存在 = 空表（Task 3 实现）。
func (s *Store) load() error { return nil }

// saveLocked 原子落盘；调用方必须已持 s.mu（Task 3 实现）。
func (s *Store) saveLocked() error { return nil }
