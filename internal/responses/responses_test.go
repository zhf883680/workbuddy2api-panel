package responses

import (
	"encoding/json"
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
