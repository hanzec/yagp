#!/usr/bin/env bash
set -euo pipefail

repository=$(cd -- "${BASH_SOURCE[0]%/*}/../.." && pwd)
test_dir=$(mktemp -d)
trap 'rm -rf -- "$test_dir"' EXIT
cd -- "$repository"
go build -o "$test_dir/runner" ./test/go/_runner

if [[ $(go env GOHOSTOS) != linux ]]; then
    if output=$("$test_dir/runner" 2>&1); then
        printf 'Expected the Go test runner to reject a non-Linux host\n' >&2
        exit 1
    fi
    [[ "$output" == 'Go tests require Linux; run this command on Linux or in a Linux container' ]]
    printf 'Non-Linux test runner rejection passed\n'
    exit 0
fi

mkdir "$test_dir/bin"
export YAGP_RUNNER_TEST_DIR="$test_dir"
cat > "$test_dir/bin/go" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[[ "$1" == test && "$2" == -overlay=* ]]
cp -- "${2#-overlay=}" "$YAGP_RUNNER_TEST_DIR/overlay.json"
shift 2
printf '%s\n' "$@" > "$YAGP_RUNNER_TEST_DIR/arguments"
EOF
chmod +x "$test_dir/bin/go"

PATH="$test_dir/bin:$PATH" "$test_dir/runner" -run TestStartup -count=1 ./cmd/server
[[ $(cat "$test_dir/arguments") == $'-run\nTestStartup\n-count=1\n./cmd/server' ]]
for file in cmd/server/main_test.go cmd/yagp-daemon/gost_linux_test.go internal/nodebridge/panel_test.go; do
    grep -Fq "\"$repository/$file\"" "$test_dir/overlay.json"
done
PATH="$test_dir/bin:$PATH" "$test_dir/runner"
[[ $(cat "$test_dir/arguments") == ./... ]]
printf 'Linux test overlay and argument forwarding checks passed\n'
