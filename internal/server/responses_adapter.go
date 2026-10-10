// responses_adapter.go Responses 响应适配器：把内层 chatCompletions 写出的
// chat completions 字节（SSE 帧或完整 JSON）翻译成 OpenAI Responses 输出。
//
// 为什么用适配器而不是改 chatCompletions：代理层的选号/轮换/冷却/DSML 修复等
// 行为只与**上游协议**（chat completions）有关，与客户端协议无关。在它外面套一层
// 协议适配，既能零改动复用全部既有行为（含密钥绑定、粘性护栏、用量记账），
// 又把新引入的回归风险限制在本文件内。
package server

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/responses"
)

// responsesAdapter 实现 http.ResponseWriter：吞掉内层头部/状态码，
// 把 body 按 Responses 协议翻译后写给真实客户端。
type responsesAdapter struct {
	inner   http.ResponseWriter
	stream  bool
	model   string
	origReq []byte // 客户端原始 Responses 请求（翻译器需要它做上下文）
	chatReq []byte // 翻译后的 chat 请求
	state   responses.State

	header http.Header // 内层看到的头部（不转发给真实客户端）
	status int
	buf    bytes.Buffer
	done   bool // 流式：response.completed 已发出（防重复补帧）
}

func newResponsesAdapter(inner http.ResponseWriter, stream bool, model string, origReq, chatReq []byte) *responsesAdapter {
	return &responsesAdapter{
		inner: inner, stream: stream, model: model,
		origReq: origReq, chatReq: chatReq, header: http.Header{},
	}
}

func (a *responsesAdapter) Header() http.Header { return a.header }

// WriteHeader 只记录内层状态码：2xx 不写真实头（真正输出时刻由适配器决定），
// 4xx/5xx 留到 Finish 统一映射成 Responses 错误形状。
func (a *responsesAdapter) WriteHeader(code int) {
	if a.status == 0 {
		a.status = code
	}
}

func (a *responsesAdapter) Write(p []byte) (int, error) {
	if a.status == 0 {
		a.status = http.StatusOK
	}
	// 错误体与非流式响应：整段缓冲，Finish 时统一处理。
	if a.status >= http.StatusBadRequest || !a.stream {
		a.buf.Write(p)
		return len(p), nil
	}
	// 流式：按 \n\n 跨 Write 分帧（内层/网络都可能切在半个帧中间）。
	a.buf.Write(p)
	for {
		idx := bytes.Index(a.buf.Bytes(), []byte("\n\n"))
		if idx < 0 {
			break
		}
		frame := make([]byte, idx+2)
		if _, err := a.buf.Read(frame); err != nil {
			break
		}
		if err := a.emit(frame); err != nil {
			return len(p), err
		}
	}
	return len(p), nil
}

// emit 翻译一条 chat SSE 帧并写出事件（库输出已是 event+data 原文）。
func (a *responsesAdapter) emit(frame []byte) error {
	for _, ev := range a.state.FromChatChunk(a.model, a.origReq, a.chatReq, frame) {
		if len(ev) == 0 {
			continue
		}
		if bytes.Contains(ev, []byte(`"type":"response.completed"`)) {
			a.done = true
		}
		if err := a.writeReal("text/event-stream", ev); err != nil {
			return err
		}
	}
	return nil
}

// Flush 供内层（upstream.StreamHint）逐帧刷新用。
func (a *responsesAdapter) Flush() {
	if f, ok := a.inner.(http.Flusher); ok {
		f.Flush()
	}
}

// writeReal 首次输出时写真实响应头并写出 body。
func (a *responsesAdapter) writeReal(contentType string, body []byte) error {
	h := a.inner.Header()
	h.Set("Content-Type", contentType)
	status := a.status
	if status == 0 {
		status = http.StatusOK
	}
	a.inner.WriteHeader(status)
	_, err := a.inner.Write(body)
	return err
}

// Finish 收尾：错误映射 / 非流式整体翻译 / 流式保证 response.completed。
func (a *responsesAdapter) Finish() {
	switch {
	case a.status >= http.StatusBadRequest:
		a.finishError()
	case a.stream:
		// 关键约束（实测）：库只在收到 data: [DONE] 时才发 response.completed。
		// 上游没发 [DONE]（异常截断/空流）时这里补一帧，否则 SDK 的流永远等不到结束。
		if !a.done {
			_ = a.emit([]byte("data: [DONE]\n\n"))
		}
	default:
		out := a.state.FromChatBody(a.model, a.origReq, a.chatReq, a.buf.Bytes())
		_ = a.writeReal("application/json", out)
	}
}

// finishError 把内层 OpenAI 错误体映射成 Responses 错误形状（状态码原样透传）。
func (a *responsesAdapter) finishError() {
	var in struct {
		Error struct {
			Message     string `json:"message"`
			Type        string `json:"type"`
			Code        string `json:"code"`
			GatewayHint string `json:"gateway_hint"`
		} `json:"error"`
	}
	_ = json.Unmarshal(a.buf.Bytes(), &in)

	typ := "invalid_request_error"
	if a.status >= http.StatusInternalServerError {
		typ = "server_error"
	}
	errObj := map[string]any{"message": in.Error.Message, "type": typ}
	if in.Error.Code != "" {
		errObj["code"] = in.Error.Code
	}
	// gateway_hint 是本地调度信息（哪些号被挡、何时解封），保留下来便于排障。
	if in.Error.GatewayHint != "" {
		errObj["gateway_hint"] = in.Error.GatewayHint
	}
	if in.Error.Message == "" {
		errObj["message"] = http.StatusText(a.status)
	}
	body, err := json.Marshal(map[string]any{"error": errObj})
	if err != nil {
		body = []byte(`{"error":{"message":"internal error","type":"server_error"}}`)
	}
	_ = a.writeReal("application/json", body)
}
