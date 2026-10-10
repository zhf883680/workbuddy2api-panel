package responses

import "testing"

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
