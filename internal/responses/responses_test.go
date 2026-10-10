package responses

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTranslatorsRegistered(t *testing.T) {
	if !Available() {
		t.Fatal("CLIProxyAPI 翻译器未注册（openai-response <-> openai）")
	}
}

func TestModelFromBody(t *testing.T) {
	if got := ModelFromBody([]byte(`{"model":"global:glm-5.2","stream":true}`)); got != "global:glm-5.2" {
		t.Fatalf("model = %q", got)
	}
	if got := ModelFromBody([]byte(`{}`)); got != "" {
		t.Fatalf("空体应返回空串，得到 %q", got)
	}
	if got := ModelFromBody([]byte(`not json`)); got != "" {
		t.Fatalf("坏 JSON 应返回空串，得到 %q", got)
	}
}

const sampleResponsesReq = `{"model":"glm-5.2","instructions":"你是助手","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"北京天气"}]}],"tools":[{"type":"function","name":"get_weather","description":"查天气","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}],"tool_choice":"auto","reasoning":{"effort":"high"},"max_output_tokens":123,"stream":true}`

func TestToChat(t *testing.T) {
	out := ToChat("glm-5.2", []byte(sampleResponsesReq), true)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("翻译结果不是合法 JSON: %v (%s)", err, out)
	}
	msgs, _ := m["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("应有 system + user 两条消息，得到 %v", m["messages"])
	}
	sys, _ := msgs[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "你是助手" {
		t.Fatalf("instructions 未转 system: %v", sys)
	}
	user, _ := msgs[1].(map[string]any)
	if user["role"] != "user" {
		t.Fatalf("第二条应是 user: %v", user)
	}
	if m["reasoning_effort"] != "high" {
		t.Fatalf("reasoning.effort 未转 reasoning_effort: %v", m["reasoning_effort"])
	}
	if m["max_tokens"] != float64(123) {
		t.Fatalf("max_output_tokens 未转 max_tokens: %v", m["max_tokens"])
	}
	tools, _ := m["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools 未透传: %v", m["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	fn, _ := tool["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Fatalf("tools 形状不对: %v", tools[0])
	}
	if m["model"] != "glm-5.2" {
		t.Fatalf("model 未保留: %v", m["model"])
	}
	if m["stream"] != true {
		t.Fatalf("stream 未保留: %v", m["stream"])
	}
}

// 多轮工具回传（Codex 的基石）：function_call + function_call_output
// → assistant.tool_calls + role:tool 消息。
func TestToChatToolRoundTrip(t *testing.T) {
	req := `{"model":"m","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"天气"}]},{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"北京\"}"},{"type":"function_call_output","call_id":"call_1","output":"晴 25 度"}]}`
	out := ToChat("m", []byte(req), true)
	var m struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Messages) != 3 {
		t.Fatalf("应有 3 条消息，得到 %d: %s", len(m.Messages), out)
	}
	if m.Messages[1]["role"] != "assistant" || m.Messages[1]["tool_calls"] == nil {
		t.Fatalf("function_call 未转 assistant.tool_calls: %v", m.Messages[1])
	}
	if m.Messages[2]["role"] != "tool" || m.Messages[2]["tool_call_id"] != "call_1" {
		t.Fatalf("function_call_output 未转 role:tool: %v", m.Messages[2])
	}
}

// 多模态：input_image → chat 的 image_url content part。
func TestToChatImage(t *testing.T) {
	req := `{"model":"m","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"看图"},{"type":"input_image","image_url":"data:image/png;base64,AAA"}]}]}`
	out := ToChat("m", []byte(req), true)
	var m struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Messages) != 1 {
		t.Fatalf("消息数 = %d: %s", len(m.Messages), out)
	}
	parts, _ := m.Messages[0]["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("content parts = %v", m.Messages[0]["content"])
	}
	img, _ := parts[1].(map[string]any)
	if img["type"] != "image_url" {
		t.Fatalf("input_image 未转 image_url: %v", img)
	}
}

// 库会丢弃的字段：不报错，也不出现在 chat body 里（已知限制，见设计 §5.2）。
func TestToChatDropsUnsupportedFields(t *testing.T) {
	req := `{"model":"m","input":"hi","store":true,"previous_response_id":"resp_1","tools":[{"type":"web_search"}],"reasoning":{"effort":"low","summary":"auto"},"include":["reasoning.encrypted_content"],"text":{"verbosity":"low"},"metadata":{"a":"b"},"stream":true}`
	out := ToChat("m", []byte(req), true)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("翻译结果不是合法 JSON: %v (%s)", err, out)
	}
	for _, k := range []string{"store", "previous_response_id", "include", "metadata", "text", "max_output_tokens"} {
		if _, ok := m[k]; ok {
			t.Fatalf("%s 不应出现在 chat body: %s", k, out)
		}
	}
	// tools 里的非 function 工具被丢弃后，tools 字段应为空/缺失，但请求本身照常翻译。
	if tools, ok := m["tools"].([]any); ok && len(tools) != 0 {
		t.Fatalf("web_search 应被丢弃，得到 %v", tools)
	}
	if m["reasoning_effort"] != "low" {
		t.Fatalf("reasoning.effort 仍应保留: %v", m["reasoning_effort"])
	}
}

// 裸字符串 input + 无 instructions 的最小请求。
func TestToChatMinimal(t *testing.T) {
	out := ToChat("m", []byte(`{"model":"m","input":"hi","stream":false}`), false)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("非法 JSON: %v (%s)", err, out)
	}
	if m["stream"] != false {
		t.Fatalf("stream 应为 false: %v", m["stream"])
	}
	msgs, _ := m["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", m["messages"])
	}
}

const chatStreamLines = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你好\"}}]}\n" +
	"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n"

const chatNonStreamBody = `{"id":"c1","object":"chat.completion","created":1,"model":"glm-5.2","choices":[{"index":0,"message":{"role":"assistant","content":"北京晴"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`

// collectTypes 从事件原文里抽出 type 字段（库输出为 `event: X\ndata: {...}`）。
func collectTypes(events [][]byte) []string {
	var types []string
	for _, ev := range events {
		s := string(ev)
		i := strings.Index(s, `"type":"`)
		if i < 0 {
			continue
		}
		rest := s[i+8:]
		if j := strings.IndexByte(rest, '"'); j > 0 {
			types = append(types, rest[:j])
		}
	}
	return types
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// 关键约束：response.completed 只在收到 data: [DONE] 时才由库发出 ——
// 适配器必须保证 [DONE] 一定被喂进来，否则 SDK 的流永远等不到结束事件。
func TestStreamEventsRequireDone(t *testing.T) {
	chatReq := ToChat("glm-5.2", []byte(sampleResponsesReq), true)

	var st State
	var without []string
	for _, line := range strings.Split(strings.TrimSpace(chatStreamLines), "\n") {
		without = append(without, collectTypes(st.FromChatChunk("glm-5.2", []byte(sampleResponsesReq), chatReq, []byte(line+"\n")))...)
	}
	for _, want := range []string{"response.created", "response.in_progress", "response.output_item.added", "response.output_text.delta"} {
		if !contains(without, want) {
			t.Fatalf("缺少事件 %s（实际 %v）", want, without)
		}
	}
	if contains(without, "response.completed") {
		t.Fatal("无 [DONE] 时不该出现 response.completed（这正是适配器要补帧的原因）")
	}

	with := collectTypes(st.FromChatChunk("glm-5.2", []byte(sampleResponsesReq), chatReq, []byte("data: [DONE]\n")))
	if !contains(with, "response.completed") {
		t.Fatalf("[DONE] 必须触发 response.completed，得到 %v", with)
	}
}

// usage 必须映射到 Responses 的 usage 字段。
func TestStreamUsageMapping(t *testing.T) {
	chatReq := ToChat("glm-5.2", []byte(sampleResponsesReq), true)
	var st State
	var all strings.Builder
	for _, line := range strings.Split(strings.TrimSpace(chatStreamLines), "\n") {
		for _, ev := range st.FromChatChunk("glm-5.2", []byte(sampleResponsesReq), chatReq, []byte(line+"\n")) {
			all.Write(ev)
		}
	}
	for _, ev := range st.FromChatChunk("glm-5.2", []byte(sampleResponsesReq), chatReq, []byte("data: [DONE]\n")) {
		all.Write(ev)
	}
	out := all.String()
	if !strings.Contains(out, `"input_tokens":3`) || !strings.Contains(out, `"output_tokens":2`) {
		t.Fatalf("usage 未映射（期望 input_tokens=3 / output_tokens=2）：%s", out)
	}
}

// 工具调用增量：function_call item 与 arguments 增量都要出现。
func TestStreamToolCallEvents(t *testing.T) {
	chatReq := ToChat("m", []byte(sampleResponsesReq), true)
	var st State
	var types []string
	frames := []string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"北京\"}"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`,
		`data: [DONE]`,
	}
	for _, f := range frames {
		types = append(types, collectTypes(st.FromChatChunk("m", []byte(sampleResponsesReq), chatReq, []byte(f+"\n")))...)
	}
	for _, want := range []string{"response.output_item.added", "response.function_call_arguments.delta", "response.function_call_arguments.done"} {
		if !contains(types, want) {
			t.Fatalf("工具调用流缺少 %s（实际 %v）", want, types)
		}
	}
}

// 非流式：输出完整 response 对象。
func TestNonStreamResponseObject(t *testing.T) {
	chatReq := ToChat("glm-5.2", []byte(sampleResponsesReq), false)
	var st State
	out := st.FromChatBody("glm-5.2", []byte(sampleResponsesReq), chatReq, []byte(chatNonStreamBody))
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("非流式翻译结果不是合法 JSON: %v (%s)", err, out)
	}
	if m["object"] != "response" {
		t.Fatalf("object 应为 response: %v", m["object"])
	}
	if m["status"] != "completed" {
		t.Fatalf("status 应为 completed: %v", m["status"])
	}
	outArr, _ := m["output"].([]any)
	if len(outArr) == 0 {
		t.Fatalf("output 不应为空: %s", out)
	}
}
