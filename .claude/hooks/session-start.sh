#!/bin/bash
# SessionStart hook for Claude Code on the web (registered in .claude/settings.json).
#
# A web session starts from an empty container. `make setup` installs what the test
# tiers need: the Go module cache, setup-envtest, and the envtest control-plane binaries
# (kube-apiserver, etcd) for the Kubernetes version pinned in the Makefile. It is
# idempotent, so re-running it on resume is cheap.
set -euo pipefail

if [ "${CLAUDE_CODE_REMOTE:-}" != "true" ]; then
  exit 0
fi

cd "${CLAUDE_PROJECT_DIR:-$(dirname "$0")/../..}"
export GOTOOLCHAIN=auto

make setup

# Let ad-hoc `go test -tags envtest` runs find the control plane without going
# through make. The file is shared with other hooks, so append once, never truncate.
if [ -n "${CLAUDE_ENV_FILE:-}" ] && ! grep -q '^export KUBEBUILDER_ASSETS=' "$CLAUDE_ENV_FILE" 2>/dev/null; then
  echo "export KUBEBUILDER_ASSETS=\"$(make -s assets-path)\"" >> "$CLAUDE_ENV_FILE"
  echo 'export GOTOOLCHAIN=auto' >> "$CLAUDE_ENV_FILE"
fi
