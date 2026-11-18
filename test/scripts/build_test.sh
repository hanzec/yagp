#!/usr/bin/env bash
set -euo pipefail

repository=$(cd -- "${BASH_SOURCE[0]%/*}/../.." && pwd)
test_dir=$(mktemp -d)
trap 'rm -rf -- "$test_dir"' EXIT
mkdir -p "$test_dir/bin" "$test_dir/project/web"
export YAGP_BUILD_TEST_DIR="$test_dir"

# Exercise the real Makefile while keeping compilation and npm installation out
# of the fixture. The server must see the completed frontend, even with -j.
cat > "$test_dir/bin/npm" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$YAGP_BUILD_TEST_DIR/npm-commands"
case "$*" in
    ci) ;;
    'run build')
        sleep 0.2
        touch "$YAGP_BUILD_TEST_DIR/web-built"
        ;;
    *) printf 'Unexpected npm command: %s\n' "$*" >&2; exit 1 ;;
esac
EOF
cat > "$test_dir/bin/go" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[[ "$1" == build && "$CGO_ENABLED" == 0 && "$GOOS" == linux ]]
while (($#)); do
    if [[ "$1" == -o ]]; then
        output=$2
        if [[ "$output" == yagp && ! -f "$YAGP_BUILD_TEST_DIR/web-built" ]]; then
            printf 'Server build started before the frontend was ready\n' >&2
            exit 1
        fi
        printf '%s\n' "$output" >> "$YAGP_BUILD_TEST_DIR/go-outputs"
        exit 0
    fi
    shift
done
exit 1
EOF
chmod +x "$test_dir/bin/npm" "$test_dir/bin/go"

for target in build build-server; do
    rm -f "$test_dir/web-built" "$test_dir/npm-commands" "$test_dir/go-outputs"
    PATH="$test_dir/bin:$PATH" make --no-print-directory --file "$repository/Makefile" \
        --directory "$test_dir/project" --jobs 3 "$target" VERSION=review-test
    [[ $(cat "$test_dir/npm-commands") == $'ci\nrun build' ]]
    [[ $(grep -Fxc yagp "$test_dir/go-outputs") == 1 ]]
    if [[ "$target" == build ]]; then
        [[ $(grep -Fxc yagp-daemon "$test_dir/go-outputs") == 1 ]]
    else
        [[ $(wc -l < "$test_dir/go-outputs") -eq 1 ]]
    fi
done
printf 'Build ordering checks passed\n'
