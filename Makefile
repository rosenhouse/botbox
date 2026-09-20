# Test tiers are DESIGN.md §11. Tool and control-plane pins live here, one
# variable each; bumping one is its own pull request.

ENVTEST_K8S_VERSION ?= 1.37.0
# Tracks the controller-runtime minor version pinned in go.mod.
SETUP_ENVTEST_VERSION ?= release-0.25
# The release index setup-envtest downloads from, pinned to a controller-tools tag.
ENVTEST_INDEX_URL ?= https://raw.githubusercontent.com/kubernetes-sigs/controller-tools/v0.22.0/envtest-releases.yaml

# Project-local tool and asset directories. Both are git-ignored.
LOCALBIN := $(CURDIR)/bin
ENVTEST_ASSETS_DIR := $(LOCALBIN)/envtest
SETUP_ENVTEST := $(LOCALBIN)/setup-envtest

# Let go fetch the toolchain go.mod asks for.
GOTOOLCHAIN ?= auto
export GOTOOLCHAIN

# Prints the KUBEBUILDER_ASSETS directory for the pinned version.
ENVTEST_USE := $(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --index $(ENVTEST_INDEX_URL) --bin-dir $(ENVTEST_ASSETS_DIR) -p path

.PHONY: help
help:
	@echo "Targets:"
	@echo "  setup         Download modules and install the envtest control plane."
	@echo "  assets-path   Print the KUBEBUILDER_ASSETS directory and nothing else."
	@echo "  test          Run the unit tier. No API server."
	@echo "  test-envtest  Run the envtest tier."
	@echo "  fmt           Fail if any file needs gofmt."
	@echo "  vet           Run go vet over both tiers."

.PHONY: setup
setup: $(SETUP_ENVTEST)
	go mod download
	@$(ENVTEST_USE) >/dev/null
	@echo "The envtest control plane for Kubernetes $(ENVTEST_K8S_VERSION) is installed."

$(SETUP_ENVTEST):
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION)

.PHONY: assets-path
assets-path: $(SETUP_ENVTEST)
	@$(ENVTEST_USE)

.PHONY: test
test:
	go test ./...

.PHONY: test-envtest
test-envtest: setup
	KUBEBUILDER_ASSETS="$$($(ENVTEST_USE))" go test -tags envtest -count=1 ./...

.PHONY: fmt
fmt:
	@files=$$(gofmt -l . | grep -v '^bin/' || true); \
	if [ -n "$$files" ]; then \
		echo "These files need gofmt:"; \
		echo "$$files"; \
		exit 1; \
	fi

.PHONY: vet
vet:
	go vet -tags envtest ./...
