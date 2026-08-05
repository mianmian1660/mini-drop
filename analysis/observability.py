"""Small JSON-line logging helpers for analyzer observability."""

import json
import re
import sys
import time
from datetime import datetime, timezone

_DSN_PASSWORD_RE = re.compile(r"(?i)(password=)([^ \t\n\r]+)")
_URL_SECRET_RE = re.compile(r"(?i)([?&](token|access_token|secret|signature|X-Amz-Signature)=)([^&\s]+)")
_BEARER_RE = re.compile(r"(?i)(bearer\s+)([A-Za-z0-9._~+/=-]+)")


def now_seconds() -> float:
    return time.monotonic()


def elapsed_seconds(started_at: float) -> float:
    return round(max(0.0, time.monotonic() - started_at), 6)


def redact_value(value):
    if isinstance(value, str):
        value = _DSN_PASSWORD_RE.sub(r"\1***", value)
        value = _URL_SECRET_RE.sub(r"\1***", value)
        value = _BEARER_RE.sub(r"\1***", value)
        return value
    if isinstance(value, dict):
        redacted = {}
        for key, nested in value.items():
            if str(key).lower() in {"password", "secret", "secret_key", "access_key", "token", "authorization"}:
                redacted[key] = "***"
            else:
                redacted[key] = redact_value(nested)
        return redacted
    if isinstance(value, list):
        return [redact_value(item) for item in value]
    return value


def log_event(stage: str, **fields):
    payload = {
        "component": "analysis",
        "stage": stage,
        "ts": datetime.now(timezone.utc).isoformat(),
    }
    for key, value in fields.items():
        if value is not None:
            payload[key] = redact_value(value)
    print("[analysis_observe] " + json.dumps(payload, ensure_ascii=False, sort_keys=True),
          file=sys.stderr)
