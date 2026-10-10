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
	"encoding/json"
	"errors"
	"fmt"
	"log"
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
	// lastSaveErr 上一次落盘失败的文案（仅由刷盘 goroutine/Stop 在持锁外写读，
	// 用于同一条错误只记一次日志）。
	lastSaveErr string
	startOnce   sync.Once
	stopOnce    sync.Once
	stopCh      chan struct{}
	doneCh      chan struct{}
}

// New 构建存储并从 path 读取已有密钥（文件不存在 = 空表，不是错误）。
//
// path 会解析为绝对路径：写盘失败时的错误信息要能直接定位文件，相对路径在
// 容器/后台启动（cwd 与预期不同）下无从排查。
func New(path string) (*Store, error) {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
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
		s.snap.Store(cur) // 回滚：落盘失败时内存必须与磁盘一致（否则报错了密钥却能用，重启又消失）
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
		s.snap.Store(cur) // 回滚（同 Create）
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
		s.snap.Store(cur) // 回滚（同 Create）
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
	prev := cur
	s.snap.Store(&Snapshot{byHash: byHash, list: list})
	if err := s.saveLocked(); err != nil {
		s.snap.Store(prev) // 回滚（同 Create）
		return err
	}
	return nil
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

// file 落盘结构。
type file struct {
	Version int    `json:"version"`
	Keys    []*Key `json:"keys"`
}

// fileVersion 当前格式版本。
const fileVersion = 1

// load 读取 keys.json；文件不存在 = 空表（不是错误）。
func (s *Store) load() error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("keystore: read %s: %w", s.path, err)
	}
	if len(raw) == 0 {
		return nil
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return fmt.Errorf("keystore: parse %s: %w", s.path, err)
	}
	byHash := make(map[string]*Key, len(f.Keys))
	list := make([]*Key, 0, len(f.Keys))
	for _, k := range f.Keys {
		if k == nil || k.ID == "" || k.Hash == "" {
			continue // 非法条目直接跳过：坏一条不该让整个网关起不来
		}
		k.Mode, k.UIDs = normalize(k.Mode, k.UIDs)
		byHash[k.Hash] = k
		list = append(list, k)
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].CreatedAt < list[j].CreatedAt })
	s.snap.Store(&Snapshot{byHash: byHash, list: list})
	return nil
}

// saveLocked 原子落盘（唯一临时名 + rename，权限 0600）；调用方必须已持 s.mu。
//
// 为什么临时名唯一（而不是固定的 keys.json.tmp）：固定名会被上一次异常退出遗留的
// 文件（可能是别的 uid 创建、0600/0444）卡死 —— 表现为此后每次写入都 permission denied，
// 而目录本身是可写的。CreateTemp 生成唯一名，从根上消除这种"卡死"形态。
func (s *Store) saveLocked() error {
	snap := s.snap.Load()
	f := file{Version: fileVersion, Keys: snap.list}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("keystore: marshal: %w", err)
	}
	dir := filepath.Dir(s.path)
	if err := ensureDir(s.path); err != nil {
		return fmt.Errorf("keystore: 无法创建目录 %s: %w%s", dir, err, dirHint(dir))
	}
	tmpf, err := os.CreateTemp(dir, filepath.Base(s.path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("keystore: 无法写入 %s（无法创建临时文件）: %s%s", s.path, errReason(err), dirHint(dir))
	}
	tmp := tmpf.Name()
	if _, err := tmpf.Write(raw); err != nil {
		tmpf.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("keystore: 无法写入 %s（临时文件写入失败）: %s%s", s.path, errReason(err), dirHint(dir))
	}
	if err := tmpf.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("keystore: 无法写入 %s（临时文件关闭失败）: %s%s", s.path, errReason(err), dirHint(dir))
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("keystore: 替换 %s 失败: %s%s", s.path, errReason(err), dirHint(dir))
	}
	s.dirty.Store(false)
	return nil
}

// errReason 提取错误的底层原因（errno 文案），剥掉 os.CreateTemp 生成的随机文件名。
//
// 为什么必须剥掉：刷盘是周期性的，同一条错误要靠**文案稳定**来去重；只要文案里
// 含随机临时文件名，每次都会判成"新错误"，日志被同一条故障刷屏。
func errReason(err error) string {
	var perr *os.PathError
	if errors.As(err, &perr) {
		return perr.Err.Error()
	}
	return err.Error()
}

// dirHint 在落盘失败时补充可执行的排查信息：目录当前是否真的不可写、进程身份、
// 以及容器部署下最常见的两种处置方式。写盘失败在远端（docker 挂载目录属主与容器
// 用户不一致）几乎总是同一类原因，光靠 "permission denied" 无法定位到该改什么。
func dirHint(dir string) string {
	writable := "否"
	if f, err := os.CreateTemp(dir, ".wb2a-write-probe-*"); err == nil {
		writable = "是（说明不是目录权限问题，可能是同名的遗留只读临时文件）"
		_ = f.Close()
		_ = os.Remove(f.Name())
	} else {
		writable = "否（" + errReason(err) + "）"
	}
	idPart := ""
	if uid, gid, ok := processIDs(); ok {
		idPart = fmt.Sprintf("；进程 uid=%d gid=%d", uid, gid)
	}
	return fmt.Sprintf("（目录 %s 可写：%s%s。docker compose 部署通常是宿主机目录属主与容器用户不一致："+
		"sudo chown -R 10001:10001 ./data，或 PUID=$(id -u) PGID=$(id -g) docker compose up -d）",
		dir, writable, idPart)
}

// NoteUsage 累计一次上游尝试的用量（与 internal/usage 同口径：重试与失败都计）。
func (s *Store) NoteUsage(id string, ok bool, promptTokens, completionTokens int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.snap.Load()
	old, found := findLocked(cur, id)
	if !found {
		return
	}
	next := *old
	next.Usage.Req++
	if !ok {
		next.Usage.Err++
	}
	next.Usage.PromptTokens += promptTokens
	next.Usage.CompletionTokens += completionTokens
	next.LastUsedAt = s.now().Unix()
	s.replaceLocked(old, &next)
	s.dirty.Store(true)
}

// Save 立即落盘（进程退出前调用）。
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

// saveAndLog 供防抖刷盘与 Stop 使用：失败必须落日志。
// 此前是 `_ = s.Save()` 静默吞掉 —— 部署里目录不可写时，密钥用量等统计会一直丢，
// 而日志里一句提示都没有（用户只能从面板操作报错才发现）。
// 同一条错误只打一次（去重）：刷盘是周期性的，重复刷屏会把日志淹掉。
func (s *Store) saveAndLog() {
	if err := s.Save(); err != nil {
		msg := err.Error()
		if s.lastSaveErr != msg {
			s.lastSaveErr = msg
			log.Printf("ERROR: [keystore] 密钥落盘失败（用量统计等变更会丢失，请检查目录权限）：%s", msg)
		}
		return
	}
	s.lastSaveErr = ""
}

// Start 启动防抖刷盘 goroutine（幂等）。keys.json 未变时不写盘。
func (s *Store) Start() {
	s.startOnce.Do(func() {
		go func() {
			defer close(s.doneCh)
			t := time.NewTicker(s.flushEvery)
			defer t.Stop()
			for {
				select {
				case <-s.stopCh:
					return
				case <-t.C:
					if !s.dirty.Load() {
						continue
					}
					s.saveAndLog()
				}
			}
		}()
	})
}

// Stop 停止刷盘并强制落盘一次（幂等）。
func (s *Store) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
		<-s.doneCh
		if s.dirty.Load() {
			s.saveAndLog()
		}
	})
}
