#!/usr/bin/env bash
set -euo pipefail
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_dir"

mode=prepare
profile=.env.eci
args=()
if [[ "${1:-}" == prepare || "${1:-}" == cache ]]; then
  mode="$1"
  shift
fi
while (($#)); do
  case "$1" in
    --env-file) profile="${2:?missing profile path}"; shift 2 ;;
    --config|--operation) args+=("$1" "${2:?missing option value}"); shift 2 ;;
    -h|--help)
      echo "Usage: $0 [prepare|cache] [--env-file PATH] [--config PATH] [--operation NAME]"
      exit 0
      ;;
    *) echo "Unknown option: $1 (see --help)" >&2; exit 2 ;;
  esac
done

if [[ ! -f "$profile" ]]; then
  echo "Deployment profile $profile is missing. Run ./scripts/start.sh --init-env first." >&2
  exit 1
fi

if [[ "$profile" -ef .env.example ]]; then
  echo 'Use a private environment profile instead of .env.example.' >&2
  exit 1
fi

if ! command -v go >/dev/null 2>&1; then
  echo 'buildeci.sh requires Go 1.26 or newer.' >&2
  exit 1
fi

cache_id="$(go run ./cmd/buildeci "$mode" --env-file "$profile" --write-profile --output cache-id ${args[@]+"${args[@]}"})"
printf 'Saved ECI_IMAGE_CACHE_ID=%s to %s\n' "$cache_id" "$profile"
