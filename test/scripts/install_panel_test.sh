#!/usr/bin/env bash
set -euo pipefail

installer="${BASH_SOURCE[0]%/*}/../../scripts/install_panel.sh"
# A valid port reaches the memory check. Stop there before any download or installation.
free() { printf 'Mem: 64\n'; }
export -f free

expect_rejection() {
    local expected=$1 output
    shift
    if output=$(bash "$installer" "$@" 2>&1) || [[ $output != *"$expected"* ]]; then
        printf 'Unexpected installer result for %s\n%s\n' "$*" "$output" >&2
        return 1
    fi
}

for port in 0 65536 99999 18446744073709551617 bad; do
    expect_rejection '端口号必须在 1 到 65535 之间' install --port "$port"
done
expect_rejection '端口号必须在 1 到 65535 之间' 65536
expect_rejection '请为 --port 指定端口号' install --port
expect_rejection '系统内存不足' 1
expect_rejection '系统内存不足' install --port 65535
expect_rejection '系统内存不足' install 08080
