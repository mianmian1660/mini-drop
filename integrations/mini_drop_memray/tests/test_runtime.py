import importlib


def test_start_is_idempotent(monkeypatch, tmp_path):
    runtime = importlib.import_module("mini_drop_memray.runtime")
    monkeypatch.setattr(runtime, "_run", lambda directory, interval: runtime._stop.wait())
    first = runtime.start(tmp_path, 1)
    thread = runtime._thread
    second = runtime.start(tmp_path, 1)
    assert runtime._thread is thread
    assert first["state"] in {"starting", "idle"}
    assert second["state"] == "starting"
    runtime.stop()


def test_identity_contains_namespace_pid_and_start_time():
    runtime = importlib.import_module("mini_drop_memray.runtime")
    assert runtime._namespace_pid() > 0
    assert runtime._start_ticks() > 0


def test_tracker_rss_interval_matches_profile_window(monkeypatch, tmp_path):
    runtime = importlib.import_module("mini_drop_memray.runtime")
    captured = {}

    class FakeTracker:
        def __init__(self, path, **kwargs):
            captured.update(kwargs)
            self.path = path

        def __enter__(self):
            open(self.path, "wb").close()
            runtime._stop.set()

        def __exit__(self, *args):
            return None

    runtime._stop.clear()
    monkeypatch.setattr(runtime.memray, "Tracker", FakeTracker)
    runtime._run(tmp_path, 60)
    runtime._stop.clear()

    assert captured["memory_interval_ms"] == 60_000
