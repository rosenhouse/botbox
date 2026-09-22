#!/bin/sh
# Exercise external-secrets against the generic invariants of DESIGN.md §6. It
# builds what it needs, so a clean checkout is enough. Arguments go to botbox:
# --seed picks the sequences it draws, and a later --runs wins over the one here.
#
# Every port the controller binds is ephemeral, so two runs may overlap. There
# is no port guard here, unlike cert-manager's quickstart (DESIGN.md §15, D28).
set -eu
cd "$(dirname "$0")/../.."

go build -o bin/botbox ./cmd/botbox
make --no-print-directory external-secrets

KUBEBUILDER_ASSETS="$(make --no-print-directory assets-path)" ./bin/botbox run \
  --target examples/external-secrets/target.yaml --runs 5 "$@"
