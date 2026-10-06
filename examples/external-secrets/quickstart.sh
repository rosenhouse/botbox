#!/bin/sh
# Exercise external-secrets with reconciler-fuzzer. It builds what it needs, so
# a clean checkout is enough. Arguments go to reconciler-fuzzer: --seed picks
# the sequences it draws, and a later --runs wins over the one here. Exit codes:
# 0, all runs passed; 1, a check failed; 2, could not test.
#
# Every port the controller binds is ephemeral, so two runs may overlap. There
# is no port guard here, unlike cert-manager's quickstart.
set -eu
cd "$(dirname "$0")/../.."

go build -o bin/reconciler-fuzzer ./cmd/reconciler-fuzzer || exit 2
make --no-print-directory external-secrets || exit 2

KUBEBUILDER_ASSETS="$(make --no-print-directory assets-path)" ./bin/reconciler-fuzzer run \
  --target examples/external-secrets/target.yaml --runs 5 "$@"
