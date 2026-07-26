"""兼容 OpenAI 风格聊天接口的轻量客户端，不额外引入依赖。"""

import json
import os
import random
import time
import urllib.error
import urllib.request


class LLMClientError(RuntimeError):
    pass


class LLMClient:
    """处理瞬时请求失败的重试，并返回聊天/工具调用响应。"""

    def __init__(self, api_url=None, api_key=None, model=None,
                 timeout_seconds=30, max_retries=2, transport=None):
        base_url = os.environ.get("LLM_API_BASE", "").rstrip("/")
        self.api_url = api_url or os.environ.get("LLM_API_URL") or (
            f"{base_url}/chat/completions" if base_url else ""
        )
        self.api_key = api_key if api_key is not None else os.environ.get("LLM_API_KEY", "")
        self.model = model or os.environ.get("LLM_MODEL", "gpt-4o-mini")
        self.timeout_seconds = timeout_seconds
        self.max_retries = max_retries
        self.transport = transport or self._request

    @property
    def enabled(self):
        return bool(self.api_url and self.api_key)

    def chat(self, messages, tools=None):
        if not self.enabled:
            raise LLMClientError("LLM 未配置：请设置 LLM_API_URL（或 LLM_API_BASE）和 LLM_API_KEY")

        payload = {"model": self.model, "messages": messages, "temperature": 0.1}
        if tools:
            payload["tools"] = tools
            payload["tool_choice"] = "auto"

        for attempt in range(self.max_retries + 1):
            try:
                response = self.transport(payload)
                choices = response.get("choices") or []
                if not choices or not isinstance(choices[0].get("message"), dict):
                    raise LLMClientError("LLM 响应缺少 choices[0].message")
                return choices[0]["message"]
            except (urllib.error.HTTPError, urllib.error.URLError, TimeoutError, LLMClientError) as exc:
                retryable = not isinstance(exc, urllib.error.HTTPError) or exc.code in (408, 409, 429, 500, 502, 503, 504)
                if not retryable or attempt >= self.max_retries:
                    raise LLMClientError(f"LLM 请求失败: {exc}") from exc
                time.sleep(min(4.0, 0.4 * (2 ** attempt)) + random.uniform(0, 0.15))

    def _request(self, payload):
        body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        request = urllib.request.Request(
            self.api_url,
            data=body,
            headers={
                "Content-Type": "application/json",
                "Authorization": f"Bearer {self.api_key}",
            },
            method="POST",
        )
        with urllib.request.urlopen(request, timeout=self.timeout_seconds) as response:
            return json.loads(response.read().decode("utf-8"))
