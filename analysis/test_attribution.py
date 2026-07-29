#!/usr/bin/env python3
"""Focused attribution tests using the project's lightweight t_* runner."""

import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))


def _top(function="hot.work"):
    return {
        "total_samples": 10,
        "self_time_top": [{"function": function, "samples": 7, "percentage": 70.0}],
    }


def t_disabled_llm_is_skipped_and_done():
    from attribution import run_attribution

    class DisabledLLM:
        enabled = False
        model = "disabled"

    result = run_attribution(None, {"tid": "t1", "type": 0}, _top(),
                             llm_client=DisabledLLM())
    assert result["status"] == "skipped"
    assert result["done"] is True


def t_unverifiable_llm_result_is_error_not_exception():
    from attribution import run_attribution

    class FakeLLM:
        enabled = True
        model = "fake"
        def chat(self, messages, tools=None):
            return {"content": json.dumps({
                "reasoning_summary": "looks plausible",
                "suggestion": "optimize another function",
                "evidence": [{"function": "not.in.topn", "detail": "made up", "source": "llm"}],
            })}

    result = run_attribution(None, {"tid": "t1", "type": 0}, _top(),
                             llm_client=FakeLLM())
    assert result["status"] == "error"
    assert result["done"] is False
    assert "可验证" in result["reasoning_summary"]


def t_tool_call_with_current_evidence_completes():
    from attribution import run_attribution

    class FakeLLM:
        enabled = True
        model = "fake"
        def __init__(self):
            self.calls = 0
        def chat(self, messages, tools=None):
            self.calls += 1
            if self.calls == 1:
                return {"tool_calls": [{
                    "id": "tool-1",
                    "function": {"name": "read_stack_context",
                                 "arguments": '{"function":"hot.work"}'},
                }]}
            return {"content": json.dumps({
                "reasoning_summary": "hot.work dominates current CPU samples.",
                "suggestion": "Inspect hot.work loop and lock behavior.",
                "evidence": [{"function": "hot.work", "detail": "70%", "source": "topn"}],
            })}

    result = run_attribution(None, {"tid": "t1", "type": 0}, _top(),
                             "main;hot.work 7\n", llm_client=FakeLLM())
    assert result["status"] == "completed"
    assert result["evidence"][0]["function"] == "hot.work"


def t_llm_exception_is_non_blocking():
    from attribution import run_attribution
    from llm_client import LLMClientError

    class FailingLLM:
        enabled = True
        model = "fake"
        def chat(self, messages, tools=None):
            raise LLMClientError("network down")

    result = run_attribution(None, {"tid": "t1", "type": 0}, _top(),
                             llm_client=FailingLLM())
    assert result["status"] == "error"
    assert result["done"] is False
    assert "network down" in result["reasoning_summary"]


if __name__ == "__main__":
    tests = [v for k, v in list(globals().items()) if k.startswith("t_") and callable(v)]
    passed = failed = 0
    for test in tests:
        try:
            test()
            print(f"  ✅ {test.__name__}")
            passed += 1
        except Exception as e:
            import traceback
            print(f"  ❌ {test.__name__}: {e}")
            traceback.print_exc()
            failed += 1
    print(f"\n{'=' * 50}")
    print(f"结果: {passed} 通过, {failed} 失败, {len(tests)} 总计")
    sys.exit(0 if failed == 0 else 1)
