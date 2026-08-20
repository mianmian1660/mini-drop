#!/usr/bin/env python3
import os
import sys
from pathlib import Path

import memray


def clean_frame(frame):
    function, filename, lineno = frame[:3]
    return f"{function} ({Path(filename).name}:{lineno})"


def main(path):
    profile = Path(path)
    profile_id = profile.name.removesuffix(".ready")
    parts = profile_id.split("-")
    pid = int(parts[1]) if len(parts) > 1 and parts[1].isdigit() else 0
    reader = memray.FileReader(str(profile))
    if not reader.metadata.has_native_traces and str(reader.metadata.file_format).lower().find("aggregated") < 0:
        raise RuntimeError("Memray profile is not aggregated allocations")
    print(f"META\t{profile_id}\t{pid}\tpython\tpython")
    for record in reader.get_high_watermark_allocation_records():
        stack = [clean_frame(frame) for frame in reversed(record.stack_trace())]
        if stack and record.size > 0:
            print(f"{';'.join(stack)} {record.size}")


if __name__ == "__main__":
    if len(sys.argv) != 2:
        raise SystemExit("usage: memray_converter.py PROFILE.ready")
    main(sys.argv[1])
