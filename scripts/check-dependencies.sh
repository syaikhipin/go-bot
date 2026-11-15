#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
if go list -deps ./cmd/host ./cmd/controller | rg 'internal/testsupport|server/internal'; then
  echo 'Production dependency boundary failed.' >&2; exit 1
fi
if go list -deps ./cmd/host | rg -i '/(go-redis|miniredis|redigo)'; then
  echo 'Redis belongs to the Controller. The Host must remain independent.' >&2; exit 1
fi
if [[ -n "$(git ls-files -- ':(glob)**/*.env' ':(glob)**/*.env.*' ':(exclude,top,literal).env.example' .local/ configs/local.yaml docs/eci-nas-validation.zh-CN.md)" ]]; then
  echo 'Private environment files, local overrides and private deployment records must not be tracked.' >&2; exit 1
fi
echo 'Dependency and local-credential boundaries: OK'
