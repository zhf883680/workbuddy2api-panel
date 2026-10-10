// keys.go 分发密钥管理接口：列表 / 创建 / 更新 / 轮换 / 删除。
//
// 安全约定：
//   - 明文只在「创建」与「轮换」的响应里返回一次；列表只回 keystore.View（不含 hash）；
//   - 鉴权沿用面板 withAuth（只认根密钥），分发密钥访问这里一律 401；
//   - uid 语法校验复用同包的 validUID（login.go），两处白名单不会漂移。
package panel

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/keystore"
)

const (
	keyNameMaxLen = 32
	keyBodyLimit  = 64 << 10
)

// keysStore 取密钥存储；未注入时写 501 并返回 nil（与既有可选依赖口径一致）。
func (p *Panel) keysStore(w http.ResponseWriter) *keystore.Store {
	if p.cfg.Keys == nil {
		writeErr(w, http.StatusNotImplemented, "keys api not available")
		return nil
	}
	return p.cfg.Keys
}

func (p *Panel) keysList(w http.ResponseWriter, r *http.Request) {
	ks := p.keysStore(w)
	if ks == nil {
		return
	}
	list := ks.List()
	if list == nil {
		list = []keystore.View{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "keys": list})
}

// keyRequest 创建/更新入参。更新是局部更新：nil 字段 = 保持不变
// （指针语义用于区分「显式传 false/空数组」与「不传」）。
type keyRequest struct {
	Name      *string   `json:"name"`
	Mode      *string   `json:"mode"`
	UIDs      *[]string `json:"uids"`
	ExpiresAt *int64    `json:"expires_at"`
	Enabled   *bool     `json:"enabled"`
}

func (p *Panel) keysCreate(w http.ResponseWriter, r *http.Request) {
	ks := p.keysStore(w)
	if ks == nil {
		return
	}
	var in keyRequest
	if !decodeKeyBody(w, r, &in) {
		return
	}
	if in.Name == nil || in.Mode == nil {
		writeErr(w, http.StatusBadRequest, "name 与 mode 必填")
		return
	}
	if err := validateKeyInput(in.Name, in.Mode, in.UIDs, in.ExpiresAt); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	enabled := true // 缺省启用：面板上"新建即生效"更符合直觉
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	var uids []string
	if in.UIDs != nil {
		uids = *in.UIDs
	}
	var exp int64
	if in.ExpiresAt != nil {
		exp = *in.ExpiresAt
	}
	view, plain, err := ks.Create(keystore.CreateInput{
		Name: strings.TrimSpace(*in.Name), Mode: *in.Mode, UIDs: uids,
		ExpiresAt: exp, Enabled: enabled,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("panel: 新建分发密钥 id=%s name=%q mode=%s 绑定=%d", view.ID, view.Name, view.Mode, len(view.UIDs))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": view, "plaintext": plain})
}

func (p *Panel) keysUpdate(w http.ResponseWriter, r *http.Request) {
	ks := p.keysStore(w)
	if ks == nil {
		return
	}
	var in keyRequest
	if !decodeKeyBody(w, r, &in) {
		return
	}
	if err := validateKeyInput(in.Name, in.Mode, in.UIDs, in.ExpiresAt); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	upd := keystore.UpdateInput{Mode: in.Mode, UIDs: in.UIDs, ExpiresAt: in.ExpiresAt, Enabled: in.Enabled}
	if in.Name != nil {
		trimmed := strings.TrimSpace(*in.Name)
		upd.Name = &trimmed
	}
	view, err := ks.Update(r.PathValue("id"), upd)
	if errors.Is(err, keystore.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "key not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": view})
}

func (p *Panel) keysRotate(w http.ResponseWriter, r *http.Request) {
	ks := p.keysStore(w)
	if ks == nil {
		return
	}
	view, plain, err := ks.Rotate(r.PathValue("id"))
	if errors.Is(err, keystore.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "key not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("panel: 轮换分发密钥 id=%s name=%q（旧明文立即失效）", view.ID, view.Name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": view, "plaintext": plain})
}

func (p *Panel) keysRemove(w http.ResponseWriter, r *http.Request) {
	ks := p.keysStore(w)
	if ks == nil {
		return
	}
	id := r.PathValue("id")
	err := ks.Remove(id)
	if errors.Is(err, keystore.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "key not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("panel: 删除分发密钥 id=%s（调用方立即 401）", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// decodeKeyBody 读并解析请求体；空体视为"全部字段不传"（更新/轮换/删除都适用）。
func decodeKeyBody(w http.ResponseWriter, r *http.Request, out *keyRequest) bool {
	raw, err := io.ReadAll(io.LimitReader(r.Body, keyBodyLimit))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return false
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return true
	}
	if err := json.Unmarshal(raw, out); err != nil {
		writeErr(w, http.StatusBadRequest, "parse body: "+err.Error())
		return false
	}
	return true
}

// validateKeyInput 校验入参；nil 字段跳过（更新场景 = 不改该字段）。
func validateKeyInput(name, mode *string, uids *[]string, expiresAt *int64) error {
	if name != nil {
		n := strings.TrimSpace(*name)
		if n == "" || len([]rune(n)) > keyNameMaxLen {
			return errors.New("名称需为 1..32 个字符")
		}
	}
	if mode != nil && *mode != keystore.ModeGlobal && *mode != keystore.ModeBound {
		return errors.New("mode 只能是 global 或 bound")
	}
	if uids != nil {
		for _, u := range *uids {
			if !validUID(u) {
				return errors.New("uid 非法: " + u)
			}
		}
	}
	if expiresAt != nil && *expiresAt != 0 && *expiresAt <= time.Now().Unix() {
		return errors.New("过期时间必须晚于当前时间（0 = 永不过期）")
	}
	return nil
}
