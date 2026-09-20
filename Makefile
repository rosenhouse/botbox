# Test tiers are DESIGN.md §11. Tool and control-plane pins live here, one
# variable each; bumping one is its own pull request.

ENVTEST_K8S_VERSION ?= 1.37.0
SETUP_ENVTEST_VERSION ?= v0.25.1
CONTROLLER_GEN_VERSION ?= v0.22.0
CERT_MANAGER_VERSION ?= v1.21.2
# The release index setup-envtest downloads from, pinned to a controller-tools tag.
ENVTEST_INDEX_URL ?= https://raw.githubusercontent.com/kubernetes-sigs/controller-tools/$(CONTROLLER_GEN_VERSION)/envtest-releases.yaml

# Project-local tool and asset directories. Both are git-ignored.
LOCALBIN := $(CURDIR)/bin
ENVTEST_ASSETS_DIR := $(LOCALBIN)/envtest
SETUP_ENVTEST := $(LOCALBIN)/setup-envtest
CERT_MANAGER_SRC := $(LOCALBIN)/cert-manager-src
CERT_MANAGER := $(LOCALBIN)/cert-manager-controller
# The stamp carries the pin, so bumping CERT_MANAGER_VERSION rebuilds.
CERT_MANAGER_STAMP := $(LOCALBIN)/cert-manager-$(CERT_MANAGER_VERSION).built
CERT_MANAGER_CRDS := examples/cert-manager/crds/cert-manager.crds.yaml

# Let go fetch the toolchain go.mod asks for.
GOTOOLCHAIN ?= auto
export GOTOOLCHAIN

# Prints the KUBEBUILDER_ASSETS directory for the pinned version.
ENVTEST_USE := $(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --index $(ENVTEST_INDEX_URL) --bin-dir $(ENVTEST_ASSETS_DIR) -p path

.PHONY: help
help:
	@echo "Targets:"
	@echo "  setup                    Download modules and install the envtest control plane."
	@echo "  assets-path              Print the KUBEBUILDER_ASSETS directory and nothing else."
	@echo "  build                    Build bin/botbox and bin/toy-widget."
	@echo "  generate                 Write the toy target's deepcopy code and CRD YAML."
	@echo "  verify-generate          Fail if a generated file is stale."
	@echo "  bug-matrix               Write docs/bug-matrix.md from the toy's seeded bugs."
	@echo "  verify-bug-matrix        Fail if docs/bug-matrix.md is stale."
	@echo "  cert-manager             Clone and build the pinned cert-manager controller."
	@echo "  cert-manager-crds        Refetch the pinned cert-manager CRD release asset."
	@echo "  cert-manager-version     Print CERT_MANAGER_VERSION and nothing else."
	@echo "  verify-cert-manager-pin  Fail if the example drifted from the pin."
	@echo "  test                     Run the unit tier. No API server."
	@echo "  test-envtest             Run the envtest tier."
	@echo "  test-example             Run the cert-manager example and its negative control."
	@echo "  fmt                      Fail if any file needs gofmt."
	@echo "  vet                      Run go vet over both tiers."

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

.PHONY: build
build:
	go build -o bin/botbox ./cmd/botbox
	go build -o bin/toy-widget ./targets/toy-widget

.PHONY: generate
generate:
	go run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION) \
		object paths=./targets/toy-widget/api/... \
		crd paths=./targets/toy-widget/api/... output:crd:artifacts:config=targets/toy-widget/crds

.PHONY: verify-generate
verify-generate: generate
	@stale=$$(git status --porcelain -- targets/toy-widget/api/v1/zz_generated.deepcopy.go targets/toy-widget/crds); \
	if [ -n "$$stale" ]; then \
		echo "Generated files are out of date. Run 'make generate' and commit the result:"; \
		echo "$$stale"; \
		exit 1; \
	fi

# One run per seeded bug of DESIGN.md §9.1, under envtest. The deadline covers
# every run of the invocation.
.PHONY: bug-matrix
bug-matrix: build setup
	KUBEBUILDER_ASSETS="$$($(ENVTEST_USE))" ./bin/botbox matrix \
		--target targets/toy-widget/target.yaml \
		--sequences targets/toy-widget/sequences \
		--out docs/bug-matrix.md \
		--deadline 8m

.PHONY: verify-bug-matrix
verify-bug-matrix: bug-matrix
	@stale=$$(git status --porcelain -- docs/bug-matrix.md); \
	if [ -n "$$stale" ]; then \
		echo "docs/bug-matrix.md is out of date. Run 'make bug-matrix' and commit the result:"; \
		git --no-pager diff -- docs/bug-matrix.md; \
		exit 1; \
	fi

# cert-manager's cmd/controller is a nested module with a replace directive and
# no tag of its own, so go install cannot reach it (DESIGN.md §15, D10).
.PHONY: cert-manager
cert-manager: $(CERT_MANAGER_STAMP)

$(CERT_MANAGER_STAMP):
	rm -rf $(CERT_MANAGER_SRC) $(LOCALBIN)/cert-manager-*.built
	git clone --depth 1 --branch $(CERT_MANAGER_VERSION) \
		https://github.com/cert-manager/cert-manager.git $(CERT_MANAGER_SRC)
	cd $(CERT_MANAGER_SRC)/cmd/controller && go build -o $(CERT_MANAGER) .
	touch $@

# Lets CI key its cache on the pin, which lives in this file alone (§11).
.PHONY: cert-manager-version
cert-manager-version:
	@echo $(CERT_MANAGER_VERSION)

# Refetches the checked-in CRDs. Bumping the pin needs this and a commit.
.PHONY: cert-manager-crds
cert-manager-crds:
	curl -fsSL -o $(CERT_MANAGER_CRDS) \
		https://github.com/cert-manager/cert-manager/releases/download/$(CERT_MANAGER_VERSION)/cert-manager.crds.yaml

# The example copies the pin into its own declaration and its CRDs, which this
# holds to the Makefile's value (§11).
.PHONY: verify-cert-manager-pin
verify-cert-manager-pin:
	@grep -qx 'version: $(CERT_MANAGER_VERSION)' examples/cert-manager/target.yaml \
		|| { echo "examples/cert-manager/target.yaml does not declare version: $(CERT_MANAGER_VERSION)."; exit 1; }
	@grep -q 'app.kubernetes.io/version: "$(CERT_MANAGER_VERSION)"' $(CERT_MANAGER_CRDS) \
		|| { echo "$(CERT_MANAGER_CRDS) is not the $(CERT_MANAGER_VERSION) release asset. Run 'make cert-manager-crds'."; exit 1; }

.PHONY: test
test:
	go test ./...

.PHONY: test-envtest
test-envtest: setup
	KUBEBUILDER_ASSETS="$$($(ENVTEST_USE))" go test -tags envtest -count=1 ./...

# The example tier of DESIGN.md §11. The negative control proves the example is
# not passing vacuously: with the owner reference off, cert-manager retains the
# issued Secret by design, and the target declares Secrets as managed, so G3
# must report it.
.PHONY: test-example
test-example: verify-cert-manager-pin
	@echo "==> the default configuration, which must pass"
	@examples/cert-manager/quickstart.sh \
		|| { echo "test-example: the default configuration failed."; exit 1; }
	@echo "==> the negative control, which must fail G3"
	@log=$$(examples/cert-manager/quickstart.sh --launch-arg --enable-certificate-owner-ref=false 2>&1); \
	status=$$?; \
	echo "$$log"; \
	if [ $$status -eq 0 ]; then \
		echo "test-example: the negative control passed, so the example proves nothing."; \
		exit 1; \
	fi; \
	if ! echo "$$log" | grep -q "G3 .*Secret example-tls"; then \
		echo "test-example: the negative control failed, but not on G3 naming the retained Secret."; \
		exit 1; \
	fi

.PHONY: fmt
fmt:
	@files=$$(gofmt -l $$(git ls-files -co --exclude-standard '*.go')); \
	if [ -n "$$files" ]; then \
		echo "These files need gofmt:"; \
		echo "$$files"; \
		exit 1; \
	fi

.PHONY: vet
vet:
	go vet -tags envtest ./...
