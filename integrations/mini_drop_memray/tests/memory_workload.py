import argparse
import time

import mini_drop_memray


def pyMemoryPeakWorker(megabytes, hold_seconds):
    blocks = []
    for _ in range(megabytes):
        blocks.append(bytearray(1024 * 1024))
        time.sleep(0.1)
    time.sleep(hold_seconds)
    return len(blocks)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--megabytes", type=int, default=24)
    parser.add_argument("--hold-seconds", type=int, default=90)
    parser.add_argument("--interval-seconds", type=int, default=10)
    args = parser.parse_args()
    mini_drop_memray.start(interval_seconds=args.interval_seconds)
    pyMemoryPeakWorker(args.megabytes, args.hold_seconds)


if __name__ == "__main__":
    main()
