#!/usr/bin/env bash
set -euo pipefail

cd -- "${BASH_SOURCE[0]%/*}/../.."
export YAGP_PUBLIC_HOST=panel.example.test AUTHENTIK_OUTPOST_URL=http://authentik.example.test
docker compose -f docker-compose.yml config --format json |
    jq -e --arg path "$PWD/backups" '.services.yagp.volumes | any(
        .type == "bind" and .source == $path and .target == "/app/backups" and (.read_only != true)
    )' >/dev/null
