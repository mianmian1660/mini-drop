"""通过受限 LLM 工具调用生成带证据约束的性能归因。"""

import json
from datetime import datetime, timezone

from llm_client import LLMClient, LLMClientError


MAX_TOOL_ROUNDS = 4
RULE_VERSION = "stage4-rules-v1"


def _json(value):
    return json.dumps(value, ensure_ascii=False, separators=(",", ":"))


def _safe_rows(conn, query, params):
    if conn is None:
        return []
    try:
        cur = conn.cursor()
        cur.execute(query, params)
        columns = [column[0] for column in cur.description]
        rows = [dict(zip(columns, row)) for row in cur.fetchall()]
        cur.close()
        return rows
    except Exception:
        return []


def _load_baselines(conn, task):
    """仅返回历史任务元数据，不向模型暴露原始采集文件。"""
    return _safe_rows(
        conn,
        """
        SELECT tid, name, type, profiler_type, created_at, analysis_status
        FROM hotmethod_tasks
        WHERE tid <> %s AND deleted_at IS NULL
          AND type = %s AND target_ip = %s AND analysis_status = 2
        ORDER BY created_at DESC LIMIT 3
        """,
        (task.get("tid", ""), task.get("type", 0), task.get("target_ip", "")),
    )


def _stack_context(folded_text, function_name, limit=5):
    matches = []
    needle = str(function_name or "").strip()
    if not needle:
        return matches
    for line in (folded_text or "").splitlines():
        if needle in line:
            matches.append(line[:1200])
            if len(matches) >= limit:
                break
    return matches


def _compare_samples(current_top, baseline_top):
    previous = {item.get("function"): item for item in (baseline_top or []) if item.get("function")}
    changes = []
    for item in (current_top or [])[:20]:
        name = item.get("function")
        old = previous.get(name, {})
        now_pct = float(item.get("percentage") or 0)
        old_pct = float(old.get("percentage") or 0)
        changes.append({
            "function": name,
            "current_percentage": round(now_pct, 2),
            "baseline_percentage": round(old_pct, 2),
            "delta_percentage": round(now_pct - old_pct, 2),
            "current_samples": int(item.get("samples") or 0),
        })
    return changes


def _redact(value):
    if isinstance(value, dict):
        out = {}
        for key, item in value.items():
            lowered = str(key).lower()
            if any(word in lowered for word in ("secret", "token", "key", "password")):
                out[key] = "***"
            elif "url" in lowered:
                out[key] = str(item).split("?", 1)[0][:160]
            else:
                out[key] = _redact(item)
        return out
    if isinstance(value, list):
        return [_redact(item) for item in value[:20]]
    if isinstance(value, str):
        return value[:400]
    return value


TOOL_SCHEMAS = [
    {"type": "function", "function": {"name": "get_history_baseline", "description": "查询同目标、同采集器的历史完成任务元数据。", "parameters": {"type": "object", "properties": {}, "additionalProperties": False}}},
    {"type": "function", "function": {"name": "compare_samples", "description": "比较当前 TopN 与调用方提供的 baseline TopN，返回可验证的占比变化。", "parameters": {"type": "object", "properties": {"baseline_top": {"type": "array", "items": {"type": "object"}}}, "additionalProperties": False}}},
    {"type": "function", "function": {"name": "read_stack_context", "description": "读取某热点函数出现的折叠调用栈上下文。", "parameters": {"type": "object", "properties": {"function": {"type": "string"}}, "required": ["function"], "additionalProperties": False}}},
]


def _tool_result(name, arguments, conn, task, top_functions, folded_text):
    if name == "get_history_baseline":
        return {"baselines": _load_baselines(conn, task)}
    if name == "compare_samples":
        return {"changes": _compare_samples(top_functions, arguments.get("baseline_top", []))}
    if name == "read_stack_context":
        function_name = arguments.get("function", "")
        return {"function": function_name, "stacks": _stack_context(folded_text, function_name)}
    return {"error": f"不允许的工具: {name}"}


def _validate_conclusion(payload, top_functions):
    if not isinstance(payload, dict):
        raise ValueError("结论必须是 JSON 对象")
    evidence = payload.get("evidence")
    if not isinstance(evidence, list) or not evidence:
        raise ValueError("结论必须含至少一条 evidence")
    known_functions = {item.get("function") for item in top_functions if item.get("function")}
    verified = []
    for item in evidence[:8]:
        if not isinstance(item, dict):
            continue
        function_name = item.get("function", "")
        if function_name and function_name not in known_functions:
            continue
        verified.append({
            "function": function_name,
            "detail": str(item.get("detail", ""))[:600],
            "source": str(item.get("source", "analysis"))[:80],
        })
    if not verified:
        raise ValueError("结论没有可由当前分析验证的证据")
    return {
        "status": "completed",
        "reasoning_summary": str(payload.get("reasoning_summary", ""))[:1200],
        "suggestion": str(payload.get("suggestion", ""))[:1600],
        "evidence": verified,
        "done": True,
    }


def run_attribution(conn, task, top_json, folded_text="", llm_client=None):
    """生成可安全展示、带证据约束的归因记录，失败时不阻塞主分析。"""
    top_functions = list((top_json or {}).get("self_time_top") or [])[:20]
    base = {
        "status": "skipped",
        "reasoning_summary": "",
        "suggestion": "",
        "evidence": [],
        "done": True,
        "engine": "",
        "model": "",
        "rule_version": RULE_VERSION,
        "generated_at": datetime.now(timezone.utc).isoformat(),
    }
    if not top_functions:
        base.update(status="skipped", reasoning_summary="没有可用于归因的热点 TopN 数据。")
        return base

    client = llm_client or LLMClient()
    base["engine"] = getattr(client, "model", "configured-llm")
    base["model"] = base["engine"]
    if not getattr(client, "enabled", True):
        base.update(status="skipped", reasoning_summary="未配置 LLM，已保留规则建议和热点证据。")
        return base

    messages = [{
        "role": "system",
        "content": "你是性能归因助手。只能依据工具和给定 TopN 下结论，不得虚构指标。完成后仅输出 JSON：{reasoning_summary,suggestion,evidence:[{function,detail,source}]}。evidence 必须引用当前热点函数。",
    }, {
        "role": "user",
        "content": _json({
            "task": {"name": task.get("name", ""), "type": task.get("type", 0), "params": _redact(task.get("request_params", {}))},
            "top_functions": top_functions,
            "total_samples": (top_json or {}).get("total_samples", 0),
            "rule_version": RULE_VERSION,
        }),
    }]

    try:
        for _ in range(MAX_TOOL_ROUNDS):
            message = client.chat(messages, tools=TOOL_SCHEMAS)
            tool_calls = message.get("tool_calls") or []
            if tool_calls:
                messages.append({"role": "assistant", "content": message.get("content") or "", "tool_calls": tool_calls})
                for call in tool_calls:
                    fn = call.get("function", {})
                    try:
                        args = json.loads(fn.get("arguments") or "{}")
                    except json.JSONDecodeError:
                        args = {}
                    result = _tool_result(fn.get("name", ""), args, conn, task, top_functions, folded_text)
                    messages.append({"role": "tool", "tool_call_id": call.get("id", ""), "content": _json(result)})
                continue
            content = message.get("content") or "{}"
            parsed = json.loads(content.strip().removeprefix("```json").removesuffix("```").strip())
            base.update(_validate_conclusion(parsed, top_functions))
            return base
        base.update(status="error", reasoning_summary="LLM 工具调用轮次超过上限，未采用未验证结论。", done=False)
    except (LLMClientError, ValueError, TypeError, json.JSONDecodeError) as exc:
        base.update(status="error", reasoning_summary=f"智能归因未生成可验证结论：{exc}", done=False)
    return base
