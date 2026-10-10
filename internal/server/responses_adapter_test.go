package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const adapterSampleReq = `{"model":"glm-5.2","input":"你好","stream":true}`

// 流式：内层状态码被吞掉，输出必须是 Responses 事件，且绝不透传 chat 原始帧。
func TestResponsesAdapterStreamsEvents(t *testing.T) {
	rec := httptest.NewRecorder()
	a := newResponsesAdapter(rec, true, "glm-5.2", []byte(adapterSampleReq), []byte(`{"model":"glm-5.2"}`))
	a.WriteHeader(http.StatusOK) // 内层 200：必须被吞掉，不写真实头
	_, _ = io.WriteString(a, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你好\"}}]}\n\n")
	a.Finish()
	out := rec.Body.String()
	if !strings.Contains(out, "event: response.created") || !strings.Contains(out, "response.output_text.delta") {
		t.Fatalf("缺少 Responses 事件：%s", out)
	}
	if strings.Contains(out, `data: {"choices"`) {
		t.Fatalf("不得把 chat 原始帧透传出去：%s", out)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
}

// headerCounter 统计真实 WriteHeader 次数：net/http 对第二次调用会打
// 「superfluous response.WriteHeader call」告警（httptest.NewRecorder 不会），
// 这里把「只能写一次头」这条约束变成断言。
type headerCounter struct {
	http.ResponseWriter
	calls int
}

func (c *headerCounter) WriteHeader(code int) {
	c.calls++
	c.ResponseWriter.WriteHeader(code)
}

// 多帧流式：真实响应头必须只写一次。回归自线上日志
// 「http: superfluous response.WriteHeader call from ...responsesAdapter」——
// writeReal 此前每帧都调一次 WriteHeader。
func TestResponsesAdapterWritesRealHeaderOnce(t *testing.T) {
	rec := httptest.NewRecorder()
	cw := &headerCounter{ResponseWriter: rec}
	a := newResponsesAdapter(cw, true, "glm-5.2", []byte(adapterSampleReq), []byte(`{"model":"glm-5.2"}`))
	a.WriteHeader(http.StatusOK) // 内层头被吞
	for i := 0; i < 5; i++ {
		_, _ = io.WriteString(a, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"片段\"}}]}\n\n")
	}
	a.Finish()
	if cw.calls != 1 {
		t.Fatalf("WriteHeader 被调用 %d 次，应恰好 1 次（重复调用会被 net/http 记为 superfluous）", cw.calls)
	}
}

// 非流式与错误映射同样只写一次头。
func TestResponsesAdapterNonStreamWritesHeaderOnce(t *testing.T) {
	rec := httptest.NewRecorder()
	cw := &headerCounter{ResponseWriter: rec}
	a := newResponsesAdapter(cw, false, "glm-5.2", []byte(`{"model":"glm-5.2","input":"你好"}`), []byte(`{"model":"glm-5.2"}`))
	_, _ = io.WriteString(a, `{"choices":[{"index":0,"message":{"content":"你好"}}]}`)
	a.Finish()
	if cw.calls != 1 {
		t.Fatalf("非流式 WriteHeader 被调用 %d 次，应恰好 1 次", cw.calls)
	}
}

// 关键：上游没发 [DONE] 时，Finish 必须兜底补上，否则 SDK 流永远等不到 response.completed。
func TestResponsesAdapterCompletesOnFinish(t *testing.T) {
	rec := httptest.NewRecorder()
	a := newResponsesAdapter(rec, true, "glm-5.2", []byte(adapterSampleReq), []byte(`{"model":"glm-5.2"}`))
	_, _ = io.WriteString(a, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你好\"}}]}\n\n")
	a.Finish()
	if !strings.Contains(rec.Body.String(), "response.completed") {
		t.Fatalf("Finish 必须保证 response.completed：%s", rec.Body.String())
	}
}

// [DONE] 已到达时不得重复补帧（否则会出两个 response.completed）。
func TestResponsesAdapterNoDoubleComplete(t *testing.T) {
	rec := httptest.NewRecorder()
	a := newResponsesAdapter(rec, true, "glm-5.2", []byte(adapterSampleReq), []byte(`{"model":"glm-5.2"}`))
	_, _ = io.WriteString(a, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你好\"}}]}\n\n")
	_, _ = io.WriteString(a, "data: [DONE]\n\n")
	a.Finish()
	// 数 `event: response.completed` 行：单个事件里 "response.completed" 会出现两次
	// （event 行 + data 里的 type 字段），按子串计数会误判。
	if n := strings.Count(rec.Body.String(), "event: response.completed"); n != 1 {
		t.Fatalf("response.completed 事件出现 %d 次（应为 1 次）：%s", n, rec.Body.String())
	}
}

// 帧跨多次 Write 也必须正确分帧（内层/网络可能切在半个帧中间）。
func TestResponsesAdapterSplitsAcrossWrites(t *testing.T) {
	rec := httptest.NewRecorder()
	a := newResponsesAdapter(rec, true, "glm-5.2", []byte(adapterSampleReq), []byte(`{"model":"glm-5.2"}`))
	_, _ = io.WriteString(a, "data: {\"choices\":[{\"index\":0,\"delta\":")
	_, _ = io.WriteString(a, "{\"content\":\"你好\"}}]}\n\n")
	a.Finish()
	if !strings.Contains(rec.Body.String(), "response.output_text.delta") {
		t.Fatalf("跨 Write 分帧失败：%s", rec.Body.String())
	}
}

// 非流式：内层 chat JSON → 完整 response 对象。
func TestResponsesAdapterNonStream(t *testing.T) {
	rec := httptest.NewRecorder()
	a := newResponsesAdapter(rec, false, "glm-5.2", []byte(adapterSampleReq), []byte(`{"model":"glm-5.2"}`))
	a.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(a, `{"id":"c1","object":"chat.completion","model":"glm-5.2","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	a.Finish()
	if !strings.Contains(rec.Body.String(), `"object":"response"`) {
		t.Fatalf("非流式应输出 response 对象：%s", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
}

// 错误映射：状态码原样、错误体转 Responses 形状、gateway_hint 保留。
func TestResponsesAdapterMapsError(t *testing.T) {
	rec := httptest.NewRecorder()
	a := newResponsesAdapter(rec, true, "m", []byte(adapterSampleReq), []byte(`{"model":"m"}`))
	a.WriteHeader(http.StatusServiceUnavailable)
	_, _ = io.WriteString(a, `{"error":{"message":"绑定账号 1 个均不可用：1 个账号不存在","type":"api_error","code":"no_healthy_account","gateway_hint":"no healthy account"}}`)
	a.Finish()
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码应原样透传，得到 %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "no_healthy_account") || !strings.Contains(body, "绑定账号") {
		t.Fatalf("错误体未映射：%s", body)
	}
	if !strings.Contains(body, "gateway_hint") {
		t.Fatalf("gateway_hint 应保留：%s", body)
	}
	if !strings.Contains(body, `"type":"server_error"`) {
		t.Fatalf("5xx 应映射成 server_error：%s", body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
}

// 4xx 错误：type 为 invalid_request_error，状态码原样。
func TestResponsesAdapterMapsClientError(t *testing.T) {
	rec := httptest.NewRecorder()
	a := newResponsesAdapter(rec, false, "m", []byte(adapterSampleReq), []byte(`{"model":"m"}`))
	a.WriteHeader(http.StatusUnauthorized)
	_, _ = io.WriteString(a, `{"error":{"message":"missing or invalid API key","type":"api_error","code":"invalid_api_key"}}`)
	a.Finish()
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"type":"invalid_request_error"`) {
		t.Fatalf("4xx 应映射成 invalid_request_error：%s", rec.Body.String())
	}
}

// 适配器必须实现 http.Flusher（内层 StreamHint 靠它逐帧刷新）。
func TestResponsesAdapterImplementsFlusher(t *testing.T) {
	a := newResponsesAdapter(httptest.NewRecorder(), true, "m", []byte(adapterSampleReq), []byte(`{}`))
	if _, ok := any(a).(http.Flusher); !ok {
		t.Fatal("适配器必须实现 http.Flusher")
	}
}
