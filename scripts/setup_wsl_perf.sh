#!/usr/bin/env bash
# Build a perf binary matching the running Microsoft WSL kernel line.
# It intentionally installs only /usr/local/bin/perf, never replacing apt's
# /usr/bin/perf wrapper or the WSL kernel itself.
set -euo pipefail

kernel="$(uname -r)"
case "$kernel" in
  *microsoft*6.6*|6.6.*microsoft*) branch="linux-msft-wsl-6.6.y" ;;
  *microsoft*6.1*|6.1.*microsoft*) branch="linux-msft-wsl-6.1.y" ;;
  *)
    echo "Unsupported WSL kernel for automatic mapping: $kernel" >&2
    echo "Set WSL_PERF_BRANCH to a branch from microsoft/WSL2-Linux-Kernel." >&2
    exit 2
    ;;
esac
branch="${WSL_PERF_BRANCH:-$branch}"
workdir="${WSL_PERF_WORKDIR:-/tmp/mini-drop-wsl-perf}"

if [[ "${EUID}" -eq 0 ]]; then
  echo "Run this script as a normal sudo-capable user, not as root." >&2
  exit 2
fi

echo "[wsl-perf] kernel=$kernel branch=$branch"
sudo apt-get update
sudo apt-get install -y --no-install-recommends \
  build-essential flex bison pkg-config libssl-dev libelf-dev libdw-dev \
  libunwind-dev libbabeltrace-dev libcap-dev binutils-dev libiberty-dev git

rm -rf "$workdir"
git clone --depth 1 --branch "$branch" https://github.com/microsoft/WSL2-Linux-Kernel.git "$workdir"
make -C "$workdir/tools/perf" -j"$(nproc)"
sudo install -m 0755 "$workdir/tools/perf/perf" /usr/local/bin/perf

echo "[wsl-perf] installed $(command -v perf)"
perf --version
perf stat -e task-clock true
echo "[wsl-perf] OK. The apt-owned /usr/bin/perf was not changed."
