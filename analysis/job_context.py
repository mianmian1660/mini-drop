"""Immutable per-analysis-job context.

Context variables isolate daemon jobs from timed-out legacy worker threads. A
stale thread keeps the context captured for its own job instead of observing
process-global environment variables overwritten by the next job.
"""

from contextlib import contextmanager
from contextvars import ContextVar


_JOB_CONTEXT = ContextVar("analysis_job_context", default={})


def current() -> dict:
    return dict(_JOB_CONTEXT.get() or {})


def get(name: str, default=None):
    return (_JOB_CONTEXT.get() or {}).get(name, default)


@contextmanager
def use(values: dict):
    token = _JOB_CONTEXT.set(dict(values or {}))
    try:
        yield
    finally:
        _JOB_CONTEXT.reset(token)
