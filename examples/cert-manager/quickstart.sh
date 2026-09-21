#!/bin/sh
# Exercise cert-manager against the generic invariants of DESIGN.md §6. It
# builds what it needs, so a clean checkout is enough. Arguments go to botbox:
# --seed picks the sequences it draws, and a later --runs wins over the one here.
set -eu
cd "$(dirname "$0")/../.."

# cert-manager's healthz port is fixed (DESIGN.md §15, D28), so runs collide.
if ! command -v lsof >/dev/null; then
  echo "lsof is missing, so nothing checked whether port 9403 is free." >&2
elif lsof -nP -iTCP:9403 -sTCP:LISTEN >/dev/null; then
  echo "port 9403 is bound. cert-manager listens there, so its runs cannot overlap." >&2
  exit 1
fi

go build -o bin/botbox ./cmd/botbox
make --no-print-directory cert-manager

KUBEBUILDER_ASSETS="$(make --no-print-directory assets-path)" ./bin/botbox run \
  --target examples/cert-manager/target.yaml --runs 5 "$@"
