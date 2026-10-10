package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/keystore"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const responsesReqBody = `{"model":"glm-5.2","instructions":"你是助手","input":"你好","stream":true}`

// newFakeUpstreamCapturingBody 同 newFakeUpstream，但把上游收到的请求体交给回调
// （用来断言「客户端发 Responses、上游收到 chat completions」这条核心契约）。
func newFakeUpstreamCapturingBody(t *testing.T, behavior func(authz string, body []byte) (int, string, bool)) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			status, body, isStream := behavior(r.Header.Get("Authorization"), raw)
			ct := "application/json"
			if isStream {
				ct = "text/event-stream"
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{ct}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

// 核心契约：客户端发 Responses 请求 → 上游收到的是翻译后的 chat completions 请求；
// 客户端收到的是 Responses 事件流（不是 chat 帧）。
func TestResponsesEndpointTranslatesRequest(t *testing.T) {
	var gotBody []byte
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	up := newFakeUpstreamCapturingBody(t, func(authz string, body []byte) (int, string, bool) {
		gotBody = body
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: p, Upstream: up, APIKey: "root"})
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(responsesReqBody))
	req.Header.Set("Authorization", "Bearer root")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(gotBody, &m); err != nil {
		t.Fatalf("上游 body 非法: %v (%s)", err, gotBody)
	}
	if m["messages"] == nil {
		t.Fatalf("上游 body 未翻译成 chat 形态: %s", gotBody)
	}
	if m["model"] != "glm-5.2" {
		t.Fatalf("model 未透传: %v", m["model"])
	}
	out := rec.Body.String()
	if !strings.Contains(out, "event: response.created") || !strings.Contains(out, "event: response.completed") {
		t.Fatalf("客户端应收到完整 Responses 事件流: %s", out)
	}
	if strings.Contains(out, `data: {"id":"chatcmpl-1"`) {
		t.Fatalf("不得透传 chat 原始帧: %s", out)
	}
}

// 非流式：客户端发 stream:false → 上游仍返回 SSE（真实上游就是流式端点），
// 内层用 Aggregate 聚合，适配器再把聚合结果翻成 Responses 对象。
func TestResponsesEndpointNonStream(t *testing.T) {
	var chatBody []byte
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	up := newFakeUpstreamCapturingBody(t, func(authz string, body []byte) (int, string, bool) {
		chatBody = body
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: p, Upstream: up, APIKey: "root"})
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"glm-5.2","input":"你好","stream":false}`))
	req.Header.Set("Authorization", "Bearer root")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var sent map[string]any
	if err := json.Unmarshal(chatBody, &sent); err != nil {
		t.Fatalf("上游 body 非法: %v", err)
	}
	if sent["messages"] == nil {
		t.Fatalf("上游 body 未翻译成 chat 形态: %s", chatBody)
	}
	if !strings.Contains(rec.Body.String(), `"object":"response"`) {
		t.Fatalf("应返回 Responses 对象: %s", rec.Body.String())
	}
}

// 密钥继承：绑定密钥打 /v1/responses，绑定号不可用时 503 且带绑定明细（绝不借用其他号）。
func TestResponsesEndpointInheritsKeyBinding(t *testing.T) {
	ks, err := keystore.New(t.TempDir() + "/keys.json")
	if err != nil {
		t.Fatal(err)
	}
	_, boundPlain, err := ks.Create(keystore.CreateInput{Name: "绑定A", Mode: keystore.ModeBound, UIDs: []string{"u_a"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u_a", Nickname: "A"})
	p.Add(&auth.Auth{UID: "u_b", Nickname: "B"}) // 故意健康：绑定号不可用时也不许借
	p.Pause("u_a")
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{Pool: p, Upstream: up, APIKey: "root", Keys: ks, MaxRotate: 1})

	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(responsesReqBody))
	req.Header.Set("Authorization", "Bearer "+boundPlain)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("绑定号不可用应 503，得到 %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "绑定账号") {
		t.Fatalf("503 应带绑定明细: %s", rec.Body.String())
	}
}

// 分发密钥可用打 /v1/responses，且用量记到该密钥账上。
func TestResponsesEndpointCountsKeyUsage(t *testing.T) {
	ks, err := keystore.New(t.TempDir() + "/keys.json")
	if err != nil {
		t.Fatal(err)
	}
	_, plain, err := ks.Create(keystore.CreateInput{Name: "小王", Mode: keystore.ModeGlobal, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{Pool: p, Upstream: up, APIKey: "root", Keys: ks})

	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(responsesReqBody))
	req.Header.Set("Authorization", "Bearer "+plain)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("分发密钥应可用，得到 %d (%s)", rec.Code, rec.Body.String())
	}
	if got := ks.List()[0].Usage.Req; got == 0 {
		t.Fatalf("用量未记到密钥账上: %+v", ks.List()[0])
	}
}

// 鉴权：未带密钥 401（scopeDataPlane 与 /v1/chat/completions 同口径）。
func TestResponsesEndpointRequiresAuth(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }), APIKey: "root"})
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(responsesReqBody))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无密钥应 401，得到 %d", rec.Code)
	}
}
