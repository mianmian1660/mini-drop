"""Small JSON-line logging helpers for analyzer observability."""

import json
import sys
import time
from datetime import datetime, timezone


def now_seconds() -> float:
    return time.monotonic()


def elapsed_seconds(started_at: float) -> float:
    return round(max(0.0, time.monotonic() - started_at), 6)


def log_event(stage: str, **fields):
    payload = {
        "component": "analysis",
        "stage": stage,
        "ts": datetime.now(timezone.utc).isoformat(),
    }
    for key, value in fields.items():
        if value is not None:
            payload[key] = value
    print("[analysis_observe] " + json.dumps(payload, ensure_ascii=False, sort_keys=True),
          file=sys.stderr)
