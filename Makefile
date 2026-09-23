# Test tiers are DESIGN.md §11. Tool and control-plane pins live here, one
# variable each; bumping one is its own pull request.

ENVTEST_K8S_VERSION ?= 1.37.0
SETUP_ENVTEST_VERSION ?= v0.25.1
CONTROLLER_GEN_VERSION ?= v0.22.0
CERT_MANAGER_VERSION ?= v1.21.2
# The commit that tag names, and the sha256 of its CRD release asset. A tag can
# move, and a version label inside the asset cannot tell a patched file from
# the release.
CERT_MANAGER_COMMIT ?= 922a06aa49ee4bb802db268ef72a174af70edd32
CERT_MANAGER_CRDS_SHA256 ?= 262fef78478492cd35b73a1b227106b7802f9c056361d1f561d04b32d751337f
EXTERNAL_SECRETS_VERSION ?= v2.11.0
# The commit that tag names, and the sha256 of its CRD release asset. A tag can
# move, and this asset carries no version label at all, so the sha256 is the
# only thing that tells it from another.
EXTERNAL_SECRETS_COMMIT ?= e8f12e1f1646e0ad47966458023ff10c9577f2b0
EXTERNAL_SECRETS_CRDS_SHA256 ?= c427124642886d240af8f28d294714a168d920ef8e5d55db14600e00ed0cfb80
# The release index setup-envtest downloads from, pinned to a controller-tools tag.
ENVTEST_INDEX_URL ?= https://raw.githubusercontent.com/kubernetes-sigs/controller-tools/$(CONTROLLER_GEN_VERSION)/envtest-releases.yaml

# Each example draws its own sequences (DESIGN.md §10, M5). A pull request fixes
# the seeds, so that a failing tier means the change under review and not a new
# draw, and so a tier stays inside the ten minutes §11 budgets. The nightly
# workflow draws its own seeds. Against cert-manager, seeds 23 to 27 draw
# create, delete, recreate, restart and deleteManaged between them, and its
# negative control runs seed 23 alone, which draws a single op and so costs no
# replay to minimize.
EXAMPLE_SEED ?= 23
EXAMPLE_RUNS ?= 5
EXAMPLE_DEADLINE ?= 5m
NIGHTLY_RUNS ?= 20
NIGHTLY_DEADLINE ?= 30m

# Project-local tool and asset directories. Both are git-ignored.
LOCALBIN := $(CURDIR)/bin
ENVTEST_ASSETS_DIR := $(LOCALBIN)/envtest
SETUP_ENVTEST := $(LOCALBIN)/setup-envtest
CERT_MANAGER_REPO := https://github.com/cert-manager/cert-manager.git
CERT_MANAGER_SRC := $(LOCALBIN)/cert-manager-src
CERT_MANAGER := $(LOCALBIN)/cert-manager-controller
# The stamp carries the pin, so bumping CERT_MANAGER_VERSION rebuilds.
CERT_MANAGER_STAMP := $(LOCALBIN)/cert-manager-$(CERT_MANAGER_VERSION).built
CERT_MANAGER_CRDS := examples/cert-manager/crds/cert-manager.crds.yaml
EXTERNAL_SECRETS_REPO := https://github.com/external-secrets/external-secrets.git
EXTERNAL_SECRETS_SRC := $(LOCALBIN)/external-secrets-src
EXTERNAL_SECRETS := $(LOCALBIN)/external-secrets
EXTERNAL_SECRETS_STAMP := $(LOCALBIN)/external-secrets-$(EXTERNAL_SECRETS_VERSION).built
EXTERNAL_SECRETS_CRDS := examples/external-secrets/crds/external-secrets.yaml
# The negative control lives beside the sequences that must pass, so the tier
# runs everything else in the directory and it alone.
EXTERNAL_SECRETS_CONTROL_SEQUENCE := examples/external-secrets/sequences/orphan.json
EXTERNAL_SECRETS_SEQUENCES := $(filter-out $(EXTERNAL_SECRETS_CONTROL_SEQUENCE),$(wildcard examples/external-secrets/sequences/*.json))

# Let go fetch the toolchain go.mod asks for.
GOTOOLCHAIN ?= auto
export GOTOOLCHAIN

# Prints the KUBEBUILDER_ASSETS directory for the pinned version.
ENVTEST_USE := $(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --index $(ENVTEST_INDEX_URL) --bin-dir $(ENVTEST_ASSETS_DIR) -p path

.PHONY: help
help:
	@echo "Targets:"
	@echo "  setup                                  Download modules and install the envtest control plane."
	@echo "  assets-path                            Print the KUBEBUILDER_ASSETS directory and nothing else."
	@echo "  build                                  Build bin/botbox and bin/toy-widget."
	@echo "  generate                               Write the toy target's deepcopy code and CRD YAML."
	@echo "  verify-generate                        Fail if a generated file is stale."
	@echo "  bug-matrix                             Write docs/bug-matrix.md from the toy's seeded bugs."
	@echo "  verify-bug-matrix                      Fail if docs/bug-matrix.md is stale."
	@echo "  cert-manager                           Clone and build the pinned cert-manager controller."
	@echo "  cert-manager-crds                      Refetch the pinned cert-manager CRD release asset."
	@echo "  cert-manager-version                   Print CERT_MANAGER_VERSION and nothing else."
	@echo "  verify-cert-manager-pin                Fail if the example drifted from the pin."
	@echo "  external-secrets                       Clone and build the pinned external-secrets controller."
	@echo "  external-secrets-crds                  Refetch the pinned external-secrets CRD release asset."
	@echo "  external-secrets-version               Print EXTERNAL_SECRETS_VERSION and nothing else."
	@echo "  verify-external-secrets-pin            Fail if the example drifted from the pin."
	@echo "  test                                   Run the unit tier. No API server."
	@echo "  test-envtest                           Run the envtest tier."
	@echo "  test-example                           Run the cert-manager example and its negative control."
	@echo "  test-example-external-secrets          Run the external-secrets example and its negative control."
	@echo "  test-example-nightly                   Run the cert-manager example on seeds botbox draws."
	@echo "  test-example-external-secrets-nightly  Run the external-secrets example on seeds botbox draws."
	@echo "  fmt                                    Fail if any file needs gofmt."
	@echo "  vet                                    Run go vet over both tiers."

.PHONY: setup
setup: $(SETUP_ENVTEST)
	go mod download
	@$(ENVTEST_USE) >/dev/null
	@echo "The envtest control plane for Kubernetes $(ENVTEST_K8S_VERSION) is installed."

# Silent, because assets-path must print the path and nothing else even when
# this rule runs first.
$(SETUP_ENVTEST):
	@GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION)

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

# Each seeded bug of DESIGN.md §9.1 runs its sequence under envtest twice: under
# the bug and without it. The deadline covers every run of the invocation.
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
cert-manager: $(CERT_MANAGER)

# The binary is the target, so deleting it rebuilds, and the stamp carries the
# pin, so bumping CERT_MANAGER_VERSION rebuilds too. Building re-clones, because
# CI caches the binary and the stamp and not the clone.
$(CERT_MANAGER): $(CERT_MANAGER_STAMP)
	rm -rf $(CERT_MANAGER_SRC)
	git clone --depth 1 --branch $(CERT_MANAGER_VERSION) $(CERT_MANAGER_REPO) $(CERT_MANAGER_SRC)
	@cloned=$$(git -C $(CERT_MANAGER_SRC) rev-parse HEAD); \
	if [ "$$cloned" != "$(CERT_MANAGER_COMMIT)" ]; then \
		echo "$(CERT_MANAGER_VERSION) is $$cloned, and the pin is $(CERT_MANAGER_COMMIT). The tag moved."; \
		exit 1; \
	fi
	cd $(CERT_MANAGER_SRC)/cmd/controller && go build -o $@ .

$(CERT_MANAGER_STAMP):
	rm -f $(LOCALBIN)/cert-manager-*.built
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
	@echo "CERT_MANAGER_CRDS_SHA256 ?= $$(sha256sum $(CERT_MANAGER_CRDS) | cut -d' ' -f1)"

# Resolves a tag upstream and holds it to the pinned commit: (repository URL,
# tag, pinned commit, the variable holding it). The build's clone checks the
# same thing, and CI's cache skips the build, so this is where a moved tag
# fails (§11). An annotated tag resolves through its peeled ^{} ref, and a
# lightweight one through the ref itself.
define verify-tag-pin
	@refs=$$(git ls-remote --tags $(1) '$(2)' '$(2)^{}') \
		|| { echo "git ls-remote could not reach $(1) to resolve $(2)."; exit 1; }; \
	commit=$$(echo "$$refs" | awk '$$2 == "refs/tags/$(2)^{}" {print $$1}'); \
	[ -n "$$commit" ] || commit=$$(echo "$$refs" | awk '$$2 == "refs/tags/$(2)" {print $$1}'); \
	if [ -z "$$commit" ]; then \
		echo "$(1) carries no tag $(2), which $(4) pins to $(3)."; \
		exit 1; \
	fi; \
	if [ "$$commit" != "$(3)" ]; then \
		echo "$(2) is $$commit upstream, and $(4) is $(3). The tag moved."; \
		exit 1; \
	fi
endef

# The example declares the pin and ships the CRD release asset. This holds both
# to the Makefile's values, and the tag to the commit it named (§11).
.PHONY: verify-cert-manager-pin
verify-cert-manager-pin:
	@grep -qx 'version: $(CERT_MANAGER_VERSION)' examples/cert-manager/target.yaml \
		|| { echo "examples/cert-manager/target.yaml does not declare version: $(CERT_MANAGER_VERSION)."; exit 1; }
	@grep -q 'app.kubernetes.io/version: "$(CERT_MANAGER_VERSION)"' $(CERT_MANAGER_CRDS) \
		|| { echo "$(CERT_MANAGER_CRDS) is not the $(CERT_MANAGER_VERSION) release asset. Run 'make cert-manager-crds'."; exit 1; }
	@echo "$(CERT_MANAGER_CRDS_SHA256)  $(CERT_MANAGER_CRDS)" | sha256sum -c --status - \
		|| { echo "$(CERT_MANAGER_CRDS) hashes to $$(sha256sum $(CERT_MANAGER_CRDS) | cut -d' ' -f1). Refetch it, and pin the digest 'make cert-manager-crds' prints."; exit 1; }
	$(call verify-tag-pin,$(CERT_MANAGER_REPO),$(CERT_MANAGER_VERSION),$(CERT_MANAGER_COMMIT),CERT_MANAGER_COMMIT)

# external-secrets' main package is the repository root, with 61 replace
# directives onto in-tree modules, so go install cannot reach it either (D10).
# The fake provider registers itself only under its build tag.
.PHONY: external-secrets
external-secrets: $(EXTERNAL_SECRETS)

$(EXTERNAL_SECRETS): $(EXTERNAL_SECRETS_STAMP)
	rm -rf $(EXTERNAL_SECRETS_SRC)
	git clone --depth 1 --branch $(EXTERNAL_SECRETS_VERSION) $(EXTERNAL_SECRETS_REPO) $(EXTERNAL_SECRETS_SRC)
	@cloned=$$(git -C $(EXTERNAL_SECRETS_SRC) rev-parse HEAD); \
	if [ "$$cloned" != "$(EXTERNAL_SECRETS_COMMIT)" ]; then \
		echo "$(EXTERNAL_SECRETS_VERSION) is $$cloned, and the pin is $(EXTERNAL_SECRETS_COMMIT). The tag moved."; \
		exit 1; \
	fi
	cd $(EXTERNAL_SECRETS_SRC) && go build -tags fake -o $@ .

$(EXTERNAL_SECRETS_STAMP):
	rm -f $(LOCALBIN)/external-secrets-*.built
	touch $@

.PHONY: external-secrets-version
external-secrets-version:
	@echo $(EXTERNAL_SECRETS_VERSION)

.PHONY: external-secrets-crds
external-secrets-crds:
	curl -fsSL -o $(EXTERNAL_SECRETS_CRDS) \
		https://github.com/external-secrets/external-secrets/releases/download/$(EXTERNAL_SECRETS_VERSION)/external-secrets.yaml
	@echo "EXTERNAL_SECRETS_CRDS_SHA256 ?= $$(sha256sum $(EXTERNAL_SECRETS_CRDS) | cut -d' ' -f1)"

# The asset carries no version label, so target.yaml's declaration, the sha256
# and the tag's commit are what hold the example to the pin (§11).
.PHONY: verify-external-secrets-pin
verify-external-secrets-pin:
	@grep -qx 'version: $(EXTERNAL_SECRETS_VERSION)' examples/external-secrets/target.yaml \
		|| { echo "examples/external-secrets/target.yaml does not declare version: $(EXTERNAL_SECRETS_VERSION)."; exit 1; }
	@echo "$(EXTERNAL_SECRETS_CRDS_SHA256)  $(EXTERNAL_SECRETS_CRDS)" | sha256sum -c --status - \
		|| { echo "$(EXTERNAL_SECRETS_CRDS) hashes to $$(sha256sum $(EXTERNAL_SECRETS_CRDS) | cut -d' ' -f1). Refetch it, and pin the digest 'make external-secrets-crds' prints."; exit 1; }
	$(call verify-tag-pin,$(EXTERNAL_SECRETS_REPO),$(EXTERNAL_SECRETS_VERSION),$(EXTERNAL_SECRETS_COMMIT),EXTERNAL_SECRETS_COMMIT)

.PHONY: test
test:
	go test ./...

.PHONY: test-envtest
test-envtest: setup
	KUBEBUILDER_ASSETS="$$($(ENVTEST_USE))" go test -tags envtest -count=1 ./...

# The negative control every example tier ends with: (tier name, a command that
# must fail, the clause of the G3 statement it must fail on). A tier whose
# control passes proves nothing.
define negative-control
	@echo "==> the negative control, which must fail G3"
	@log=$$($(2) 2>&1); \
	status=$$?; \
	echo "$$log"; \
	if [ $$status -eq 0 ]; then \
		echo "$(1): the negative control passed, so the tier proves nothing."; \
		exit 1; \
	fi; \
	if ! echo "$$log" | grep -q "$(3)"; then \
		echo "$(1): the negative control failed, but not on: $(3)"; \
		exit 1; \
	fi
endef

# Each control orphans a Secret: (tier name, the control's output directory,
# the Secret's key, a pattern its value matches). objects.jsonl must mark the
# value, and no file may hold it.
define hides-the-secret
	@grep -rqF '"$(3)":"[redacted' $(2) \
		|| { echo "$(1): the control's objects.jsonl does not mark the Secret's $(3)."; exit 1; }
	@! grep -rlE '$(4)' $(2) \
		|| { echo "$(1): the files above hold the Secret's $(3)."; exit 1; }
endef

CERT_MANAGER_CONTROL_OUT = botbox-out/cert-manager-control
CERT_MANAGER_CONTROL = examples/cert-manager/quickstart.sh --seed $(EXAMPLE_SEED) --runs 1 --deadline $(EXAMPLE_DEADLINE) --out $(CERT_MANAGER_CONTROL_OUT) --launch-arg --enable-certificate-owner-ref=false
CERT_MANAGER_CONTROL_CLAUSE = G3 the v1/Secret example-tls was still there
EXTERNAL_SECRETS_CONTROL_OUT = botbox-out/external-secrets-control
EXTERNAL_SECRETS_CONTROL = KUBEBUILDER_ASSETS="$$($(ENVTEST_USE))" ./bin/botbox run --target examples/external-secrets/target.yaml --deadline $(EXAMPLE_DEADLINE) --out $(EXTERNAL_SECRETS_CONTROL_OUT) $(EXTERNAL_SECRETS_CONTROL_SEQUENCE)
EXTERNAL_SECRETS_CONTROL_CLAUSE = G3 the v1/Secret example-secret was still there

# Each example's control: (tier name). tls.key holds a PEM private key, and the
# three patterns after PRIVATE KEY are its base64 at each alignment. The fake
# provider serves s3cr3t, whose base64 is czNjcjN0, and external-secrets
# annotates the Secret with an unkeyed hash of it.
define cert-manager-control
	@rm -rf $(CERT_MANAGER_CONTROL_OUT)
	$(call negative-control,$(1),$(CERT_MANAGER_CONTROL),$(CERT_MANAGER_CONTROL_CLAUSE))
	$(call hides-the-secret,$(1),$(CERT_MANAGER_CONTROL_OUT),tls.key,PRIVATE KEY|UFJJVkFURSBL|BSSVZBVEUgS0|QUklWQVRFIEt)
endef

define external-secrets-control
	@rm -rf $(EXTERNAL_SECRETS_CONTROL_OUT)
	$(call negative-control,$(1),$(EXTERNAL_SECRETS_CONTROL),$(EXTERNAL_SECRETS_CONTROL_CLAUSE))
	$(call hides-the-secret,$(1),$(EXTERNAL_SECRETS_CONTROL_OUT),token,s3cr3t|czNjcjN0|56e1b3f734a3d8e2c7932736ca6ff7fb9a9b5a14378c70c27c5e0adf)
endef

# The example tier of DESIGN.md §11. The pinned sequences are the worked example
# of the format §7 states, so the tier runs them rather than letting them rot.
# Its control turns the owner reference off, under which cert-manager retains
# the issued Secret by design, and the target declares Secrets as managed.
.PHONY: test-example
test-example: verify-cert-manager-pin setup build
	@echo "==> the default configuration, which must pass"
	@examples/cert-manager/quickstart.sh --seed $(EXAMPLE_SEED) --runs $(EXAMPLE_RUNS) --deadline $(EXAMPLE_DEADLINE) \
		|| { echo "test-example: the default configuration failed."; exit 1; }
	@echo "==> the pinned sequences, which must pass as written"
	@KUBEBUILDER_ASSETS="$$($(ENVTEST_USE))" ./bin/botbox run \
		--target examples/cert-manager/target.yaml --deadline $(EXAMPLE_DEADLINE) \
		examples/cert-manager/sequences/*.json \
		|| { echo "test-example: a pinned sequence failed."; exit 1; }
	$(call cert-manager-control,test-example)

# The nightly tier of DESIGN.md §10 (M5). botbox draws the seeds, so a find here
# is a new one rather than the fixed seeds again, and every run prints its seed,
# so the find replays (§11). It carries the same negative control as test-example,
# because a nightly that only ever passes cannot tell a quiet night from a harness
# that stopped judging.
.PHONY: test-example-nightly
test-example-nightly: verify-cert-manager-pin
	@echo "==> drawn seeds, which must pass"
	@examples/cert-manager/quickstart.sh --runs $(NIGHTLY_RUNS) --deadline $(NIGHTLY_DEADLINE) \
		|| { echo "test-example-nightly: a drawn seed failed."; exit 1; }
	$(call cert-manager-control,test-example-nightly)

# The external-secrets example tier, in the shape of test-example. Its negative
# control is a sequence rather than a --launch-arg, because no flag makes the
# controller orphan its Secret: spec.target.creationPolicy does, and that is a
# field of the CR (DESIGN.md §15, D41).
.PHONY: test-example-external-secrets
test-example-external-secrets: verify-external-secrets-pin setup build
	@echo "==> the default configuration, which must pass"
	@examples/external-secrets/quickstart.sh --seed $(EXAMPLE_SEED) --runs $(EXAMPLE_RUNS) --deadline $(EXAMPLE_DEADLINE) \
		|| { echo "test-example-external-secrets: the default configuration failed."; exit 1; }
	@echo "==> the pinned sequences, which must pass as written"
	@test -n "$(EXTERNAL_SECRETS_SEQUENCES)" \
		|| { echo "test-example-external-secrets: no pinned sequences to run."; exit 1; }
	@KUBEBUILDER_ASSETS="$$($(ENVTEST_USE))" ./bin/botbox run \
		--target examples/external-secrets/target.yaml --deadline $(EXAMPLE_DEADLINE) \
		$(EXTERNAL_SECRETS_SEQUENCES) \
		|| { echo "test-example-external-secrets: a pinned sequence failed."; exit 1; }
	$(call external-secrets-control,test-example-external-secrets)

.PHONY: test-example-external-secrets-nightly
test-example-external-secrets-nightly: verify-external-secrets-pin setup build
	@echo "==> drawn seeds, which must pass"
	@examples/external-secrets/quickstart.sh --runs $(NIGHTLY_RUNS) --deadline $(NIGHTLY_DEADLINE) \
		|| { echo "test-example-external-secrets-nightly: a drawn seed failed."; exit 1; }
	$(call external-secrets-control,test-example-external-secrets-nightly)

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
