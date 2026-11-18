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

test_dir=$(mktemp -d)
trap 'rm -rf -- "$test_dir"' EXIT
mkdir -p "$test_dir"/{bin,config/config,release/config,systemd}
sed -e "s|^INSTALL_DIR=.*|INSTALL_DIR=$test_dir/bin|" \
    -e "s|^CONFIG_DIR=.*|CONFIG_DIR=$test_dir/config|" \
    -e "s|/etc/systemd/system/|$test_dir/systemd/|" "$installer" > "$test_dir/install.sh"
installer="$test_dir/install.sh"
export fixture_archive="$test_dir/release.tar.gz"
case $(uname -m) in
    x86_64) bin=yagp-linux-amd64 ;;
    aarch64|arm64) bin=yagp-linux-arm64 ;;
esac
if [[ -n ${YAGP_TEST_PANEL_BINARY:-} ]]; then
    cp "$YAGP_TEST_PANEL_BINARY" "$test_dir/release/$bin"
else
    go build -buildvcs=false -o "$test_dir/release/$bin" ./cmd/server
fi
cp LICENSE "$test_dir/release"
cp config/config.yaml "$test_dir/release/config/"
tar -czf "$fixture_archive" -C "$test_dir/release" .
free() { printf 'Mem: 2048\n'; }
curl() {
    if [[ $* == *' -o '* ]]; then cp "$fixture_archive" "${@: -1}"; else echo 192.0.2.1; fi
}
systemctl() {
    case "$1" in
        stop) return "${stop_result:-0}" ;;
        is-active) echo "${unit_state:-active}"; return 3 ;;
    esac
}
export -f free curl systemctl
config="$test_dir/config/config/config.yaml"
for value in '"127.0.0.1:39100"' "':39100'" ':39100' '"[::1]:39100"'; do
    printf 'server:\n  port: %s # listen\nother:\n  port: ":5151"\n' "$value" > "$config"
    output=$(bash "$installer" install --port 08080)
    case $value in
        *127.0.0.1*) expected=127.0.0.1:8080 ;;
        *::1*) expected='[::1]:8080' ;;
        *) expected=192.0.2.1:8080 ;;
    esac
    [[ $output == *"http://$expected"* ]]
    grep -Fq '# listen' "$config"
    grep -Fq ':5151' "$config"
done

# A failed config check leaves both the old config and the installed executable intact.
printf 'server: {port: invalid}\n' > "$config"
cp "$config" "$test_dir/original"
printf 'old executable\n' > "$test_dir/bin/yagp"
expect_rejection '修改监听端口失败' install --port 8080
cmp "$config" "$test_dir/original"
grep -Fxq 'old executable' "$test_dir/bin/yagp"

# An older release can still be installed without an override.
printf '#!/bin/sh\nexit 2\n' > "$test_dir/release/$bin"
tar -czf "$fixture_archive" -C "$test_dir/release" .
bash "$installer" install > "$test_dir/result"
grep -Fq '监听地址见' "$test_dir/result"

expect_rejection '修改监听端口失败' install --port 8080
cmp "$config" "$test_dir/original"
cat > "$config" <<'YAML'
server:
  port: ":39100"
other:
  port: ":39100"
YAML
output=$(bash "$installer" install --port 8080)
[[ $output == *http://192.0.2.1:8080* ]]
grep -Fqx '  port: ":8080"' "$config"
grep -Fqx '  port: ":39100"' "$config"

export stop_result=1 unit_state=active
expect_rejection '保留安装文件' uninstall
[[ -f "$test_dir/systemd/gost-panel.service" && -f "$test_dir/bin/yagp" ]]
export unit_state=inactive
bash "$installer" uninstall
[[ ! -e "$test_dir/systemd/gost-panel.service" && ! -e "$test_dir/bin/yagp" ]]
bash "$installer" uninstall
