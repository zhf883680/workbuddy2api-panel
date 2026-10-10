#!/usr/bin/env python3
"""OpenAI Responses API 冒烟测试（针对本网关的 /v1/responses）。

用途：用**真实的 openai Python SDK** 验证网关的 Responses 兼容性 —— 三个必过项：
  1. 非流式 responses.create()
  2. 流式   responses.create(stream=True)
  3. 工具调用多轮（function_call → function_call_output 回传）

用法：
    pip install openai
    scripts/responses_sdk_smoke.py --base-url http://127.0.0.1:7863 --api-key sk-xxx --model cn:glm-5.2

退出码：0 = 全部通过（未安装 openai 时打印 SKIP 也返回 0）；1 = 有失败。
"""

from __future__ import annotations

import argparse
import sys

try:
    from openai import OpenAI
except ImportError:  # 未安装 SDK：跳过，不算失败（CI 精简机不阻塞）
    print("SKIP: 未安装 openai SDK（pip install openai）")
    sys.exit(0)


def check_non_stream(client: "OpenAI", model: str) -> bool:
    resp = client.responses.create(model=model, input="用一句话说明你是谁")
    ok = getattr(resp, "object", "") == "response" and bool(getattr(resp, "output", None))
    print(f"[非流式] object={getattr(resp, 'object', None)!r} status={getattr(resp, 'status', None)!r} ok={ok}")
    if not ok:
        print(f"  原始响应: {resp}")
    return ok


def check_stream(client: "OpenAI", model: str) -> bool:
    types: list[str] = []
    text = ""
    for event in client.responses.create(model=model, input="数到三", stream=True):
        types.append(getattr(event, "type", ""))
        if getattr(event, "type", "") == "response.output_text.delta":
            text += getattr(event, "delta", "") or ""
    ok = "response.created" in types and "response.completed" in types
    print(f"[流式] 事件数={len(types)} 首={types[:3]} 末={types[-2:]} 文本={text[:40]!r} ok={ok}")
    if not ok:
        print("  response.created / response.completed 必须同时出现（否则 SDK 流式迭代会异常）")
    return ok


def check_tool_round_trip(client: "OpenAI", model: str) -> bool:
    """一轮工具调用：模型要求调用 get_weather → 我们把结果回传 → 模型给出最终回答。"""
    tools = [{
        "type": "function",
        "name": "get_weather",
        "description": "查询城市天气",
        "parameters": {
            "type": "object",
            "properties": {"city": {"type": "string"}},
            "required": ["city"],
        },
    }]
    first = client.responses.create(model=model, input="北京今天天气怎么样？", tools=tools)
    calls = [item for item in getattr(first, "output", []) if getattr(item, "type", "") == "function_call"]
    if not calls:
        print("[工具] 模型未发起 function_call（取决于模型行为，记为失败）")
        return False
    call = calls[0]
    print(f"[工具] function_call name={call.name} args={call.arguments!r} call_id={call.call_id}")

    second = client.responses.create(
        model=model,
        input=[
            {"type": "function_call", "call_id": call.call_id, "name": call.name, "arguments": call.arguments},
            {"type": "function_call_output", "call_id": call.call_id, "output": "晴，25 摄氏度"},
        ],
        tools=tools,
    )
    text = getattr(second, "output_text", "") or ""
    ok = bool(getattr(second, "output", None)) and bool(text.strip())
    print(f"[工具] 回传后 output_text={text[:60]!r} ok={ok}")
    return ok


def main() -> int:
    ap = argparse.ArgumentParser(description="OpenAI Responses API 冒烟（打本网关）")
    ap.add_argument("--base-url", default="http://127.0.0.1:7863", help="网关地址（默认 %(default)s）")
    ap.add_argument("--api-key", default="", help="网关密钥（api_key 或面板签发的分发密钥）")
    ap.add_argument("--model", default="cn:glm-5.2", help="模型名（默认 %(default)s）")
    args = ap.parse_args()

    client = OpenAI(base_url=args.base_url.rstrip("/") + "/v1", api_key=args.api_key or "unused")

    results: list[tuple[str, bool]] = []
    for name, fn in (("非流式", check_non_stream), ("流式", check_stream), ("工具多轮", check_tool_round_trip)):
        try:
            results.append((name, fn(client, args.model)))
        except Exception as exc:  # noqa: BLE001 —— 冒烟脚本要把任何异常都变成"这一项失败"
            print(f"[{name}] 异常: {type(exc).__name__}: {exc}")
            results.append((name, False))

    print("\n== 汇总 ==")
    for name, ok in results:
        print(f"  {'PASS' if ok else 'FAIL'}  {name}")
    return 0 if all(ok for _, ok in results) else 1


if __name__ == "__main__":
    sys.exit(main())
