// responses.go OpenAI Responses 端点（POST /v1/responses）。
//
// 分工：
//   - 协议翻译交给 internal/responses（CLIProxyAPI 的 sdk/translator）；
//   - 代理行为（选号/绑定/轮换/重试/冷却/DSML 修复/用量/日志）完全复用 chatCompletions ——
//     通过把翻译后的 chat body 交给它，并用 responsesAdapter 接住它的输出再翻回 Responses。
//
// 不做任何字段守卫：翻译器丢弃什么就丢什么（见设计文档 §5.2）。
package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/responses"
)

func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var peek struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &peek)
	model := responses.ModelFromBody(body)

	chatBody := responses.ToChat(model, body, peek.Stream)
	if !json.Valid(chatBody) {
		// 翻译器没能在源请求上工作（极少见）：明确报错，绝不把半成品发给上游。
		writeOpenAIError(w, http.StatusBadRequest, "translation_failed", "failed to translate responses request")
		return
	}

	adapter := newResponsesAdapter(w, peek.Stream, model, body, chatBody)
	// 复用原请求：同一 context（身份盒子 → 密钥身份/绑定集合/用量归属全部继承），
	// 同一 Header（客户端 IP / 会话头 / trace 头）。只换 body。
	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(bytes.NewReader(chatBody))
	r2.ContentLength = int64(len(chatBody))
	h.chatCompletions(adapter, r2)
	adapter.Finish()
}
