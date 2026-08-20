import atexit
import os
import threading
import time
import uuid
from pathlib import Path

import memray

_lock = threading.Lock()
_thread = None
_stop = threading.Event()
_status = {"state": "idle", "reason": ""}


def _namespace_pid():
    try:
        text = Path("/proc/self/status").read_text()
        for line in text.splitlines():
            if line.startswith("NSpid:"):
                return int(line.split()[-1])
    except (OSError, ValueError):
        pass
    return os.getpid()


def _start_ticks():
    try:
        stat = Path("/proc/self/stat").read_text()
        return int(stat[stat.rfind(")") + 2 :].split()[19])
    except (OSError, ValueError, IndexError):
        return int(time.time() * 1000)


def _retain_latest(directory, keep=3):
    complete = sorted(
        list(directory.glob("*.ready")) + list(directory.glob("*.done")),
        key=lambda path: path.stat().st_mtime,
        reverse=True,
    )
    for path in complete[keep:]:
        try:
            path.unlink()
        except FileNotFoundError:
            pass


def _run(directory, interval):
    while not _stop.is_set():
        identity = f"memray-{_namespace_pid()}-{_start_ticks()}-{uuid.uuid4()}"
        part = directory / f"{identity}.part"
        ready = directory / f"{identity}.ready"
        tracker = None
        try:
            tracker = memray.Tracker(
                str(part),
                native_traces=False,
                follow_fork=False,
                # Continuous RSS is collected by the Agent. Avoid Memray's
                # 10 ms default RSS polling and its duplicate disk records.
                memory_interval_ms=max(1000, interval * 1000),
                file_format=memray.FileFormat.AGGREGATED_ALLOCATIONS,
            )
            tracker.__enter__()
            _status.update(state="recording", reason="")
            _stop.wait(interval)
            tracker.__exit__(None, None, None)
            tracker = None
            os.replace(part, ready)
            _retain_latest(directory)
            _status.update(state="ready", reason="")
        except Exception as exc:  # Tracker conflicts and filesystem errors are diagnostics.
            _status.update(state="error", reason=f"{type(exc).__name__}: {exc}")
            if tracker is not None:
                try:
                    tracker.__exit__(None, None, None)
                except Exception:
                    pass
            try:
                part.unlink()
            except FileNotFoundError:
                pass
            _stop.wait(min(interval, 10))


def start(directory=None, interval_seconds=60):
    """Start continuous aggregated allocation windows for this process.

    This call is idempotent. Prefork servers must call it after worker creation.
    """
    global _thread
    with _lock:
        if _thread is not None and _thread.is_alive():
            return dict(_status)
        target = Path(directory or os.getenv("MINI_DROP_MEMRAY_DIR", "/tmp/mini-drop-memray"))
        target.mkdir(parents=True, exist_ok=True, mode=0o700)
        _stop.clear()
        _thread = threading.Thread(target=_run, args=(target, max(1, int(interval_seconds))), name="mini-drop-memray", daemon=True)
        _thread.start()
        _status.update(state="starting", reason="")
        return dict(_status)


def stop():
    _stop.set()
    thread = _thread
    if thread is not None and thread.is_alive():
        thread.join(timeout=5)
    return dict(_status)


atexit.register(stop)
