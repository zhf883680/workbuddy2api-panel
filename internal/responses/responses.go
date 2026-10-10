// Package responses 封装 CLIProxyAPI 的 Responses ⇄ chat completions 翻译器。
//
// 只依赖 sdk/translator 与 sdk/translator/builtin（不 import sdk/api、sdk/cliproxy，
// 避免把 gin 服务器/配置/路由拖进本二进制）。本包不解析、不校验 Responses 字段：
// 请求体原样交给翻译器，翻译器丢弃什么就丢什么（口径见设计文档 §5.2）。
package responses

import (
	"context"
	"encoding/json"

	sdktr "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator/builtin"
)

// registry 内置翻译器注册表（进程级单例，注册后只读）。
var registry = builtin.Registry()

// Available 报告两个方向的翻译器是否都已注册（启动自检 + 测试用）。
//
// 注意参数方向：库的 Has* 与 Translate* 的 (from,to) 约定**相反** ——
// 实测 HasResponseTransformer(openai, openai-response) == false，
// 而 TranslateStream/TranslateNonStream(openai → openai-response) 正常工作。
// 因此这里一律用 (openai-response, openai) 查询，与 Translate 的实参顺序不同。
func Available() bool {
	return registry.HasRequestTransformer(sdktr.FormatOpenAIResponse, sdktr.FormatOpenAI) &&
		registry.HasResponseTransformer(sdktr.FormatOpenAIResponse, sdktr.FormatOpenAI)
}

// ModelFromBody 取请求体里的 model 字段（缺失/坏 JSON 返回空串，不报错）。
func ModelFromBody(raw []byte) string {
	var peek struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(raw, &peek); err != nil {
		return ""
	}
	return peek.Model
}

// ToChat 把 Responses 请求翻译成 chat completions 请求体。
//
// model 用客户端原始串（可能带 realm 前缀）——翻译器把它原样写进 chat body，
// 再由下游 resolveModel 解析，因此 realm 路由与绑定选号语义不受影响。
func ToChat(model string, rawBody []byte, stream bool) []byte {
	return registry.TranslateRequest(sdktr.FormatOpenAIResponse, sdktr.FormatOpenAI, model, rawBody, stream)
}

// State 单个请求的响应侧状态容器（库首次调用时自行填充内部类型，调用方无需知道类型）。
type State struct{ param any }

// FromChatChunk 把一条 chat SSE 帧翻译成 0..N 个 Responses 事件原文
// （输出已是 `event: X\ndata: {...}\n\n` 形态，直接写出即可）。
func (s *State) FromChatChunk(model string, origReq, chatReq, chunk []byte) [][]byte {
	return registry.TranslateStream(context.Background(), sdktr.FormatOpenAI, sdktr.FormatOpenAIResponse,
		model, origReq, chatReq, chunk, &s.param)
}

// FromChatBody 把完整 chat completions 响应翻译成 Responses 对象。
func (s *State) FromChatBody(model string, origReq, chatReq, body []byte) []byte {
	return registry.TranslateNonStream(context.Background(), sdktr.FormatOpenAI, sdktr.FormatOpenAIResponse,
		model, origReq, chatReq, body, &s.param)
}
