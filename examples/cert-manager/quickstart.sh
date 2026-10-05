#!/bin/sh
# Exercise cert-manager with botbox. It builds what it needs, so a clean checkout
# is enough. Arguments go to botbox: --seed picks the sequences it draws, and a
# later --runs wins over the one here.
# Exit codes: 0, all runs passed; 1, a check failed; 2, could not test.
set -eu
cd "$(dirname "$0")/../.."

# cert-manager binds its healthz server to port 9403, so its runs collide.
if ! command -v lsof >/dev/null; then
  echo "lsof is missing, so nothing checked whether port 9403 is free." >&2
elif lsof -nP -iTCP:9403 -sTCP:LISTEN >/dev/null; then
  echo "port 9403 is bound. cert-manager listens there, so its runs cannot overlap." >&2
  exit 2
fi

go build -o bin/botbox ./cmd/botbox || exit 2
make --no-print-directory cert-manager || exit 2

KUBEBUILDER_ASSETS="$(make --no-print-directory assets-path)" ./bin/botbox run \
  --target examples/cert-manager/target.yaml --runs 5 "$@"
