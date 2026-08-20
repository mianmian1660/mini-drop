import os

if os.getenv("MINI_DROP_MEMRAY_ENABLED") == "1":
    from mini_drop_memray import start

    start()
