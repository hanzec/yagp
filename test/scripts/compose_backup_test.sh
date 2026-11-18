#!/usr/bin/env bash
set -euo pipefail

cd -- "${BASH_SOURCE[0]%/*}/../.."
export AUTHENTIK_OUTPOST_URL=http://authentik.example.test
check_project() {
    local host=$1
    docker compose -f docker-compose.yml config --format json |
        jq -e --arg host "$host" --arg path "$PWD/backups" '
            .services["yagp-config-init"].environment.YAGP_HOST == $host and
            .services.yagp.environment.YAGP_HOST == $host and
            (.services.yagp.labels["traefik.http.routers.yagp.rule"] | contains($host)) and
            (.services.yagp.volumes | any(
                .type == "bind" and .source == $path and .target == "/app/backups" and (.read_only != true)
            ))' >/dev/null
}

YAGP_HOST='' YAGP_PUBLIC_HOST=panel.example.test check_project panel.example.test
YAGP_HOST=legacy.example.test YAGP_PUBLIC_HOST='' check_project legacy.example.test
