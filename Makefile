# The commands CLAUDE.md and the skills refer to. Seeded from
# /opt/projects/eumaeus, trimmed to the targets this repository actually has.
#
# Anything added here gets a "## " comment on its target line, which is what
# "make help" reads.

GO                   := go
APP                  := dropin-forms
TOOLS_DIR            := $(shell $(GO) env GOPATH)/bin
GOPLS_VERSION        ?= 0.23.0
STATICCHECK_VERSION  ?= 2026.1

.DEFAULT_GOAL := help

.PHONY: help
help: ## List every target
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z0-9_-]+:.*?## / \
		{ printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

# ---------------------------------------------------------------------------
# Navigation. Both of these are gopls, which answers from the same type
# information the compiler uses -- so it knows which "New" is a method on which
# type, and it brings the doc comment with it. grep is still right for a
# setting name, a SQL column or a string in a template; this is for the
# declaration of a Go identifier, which is the thing gopls is exact about.

.PHONY: sym
sym: ## Where is a Go symbol, and what is its declaration? (make sym NAME=Submission)
	@scripts/sym "$(NAME)"

.PHONY: outline
outline: ## Every symbol in one file, with line numbers (make outline FILE=path/to/x.go)
	@if [ -z "$(FILE)" ]; then echo "usage: make outline FILE=path/to/file.go" >&2; exit 2; fi
	@command -v gopls >/dev/null || { echo "gopls is not installed. Run: make dev-tools" >&2; exit 1; }
	@# Rewritten into "file:line:col  name  kind", because gopls prints the
	@# position last and without the file -- and a position that is not a
	@# whole address is one somebody has to assemble by hand every time.
	@gopls symbols "$(FILE)" | awk -v f="$(FILE)" ' \
		{ \
			where = $$NF; sub(/-.*/, "", where); \
			$$NF = ""; sub(/ +$$/, "", $$0); \
			printf "%-52s %s\n", f ":" where, $$0 \
		} \
	'

.PHONY: dev-tools
dev-tools: ## Install the Go tooling this Makefile navigates and checks with (gopls, staticcheck)
	@# Pinned, so that two machines get the same answers.
	@GOBIN=$(TOOLS_DIR) $(GO) install golang.org/x/tools/gopls@v$(GOPLS_VERSION)
	@echo "installed $(TOOLS_DIR)/gopls ($$($(TOOLS_DIR)/gopls version | head -1))"
	@GOBIN=$(TOOLS_DIR) $(GO) install honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION)
	@echo "installed $(TOOLS_DIR)/staticcheck ($$($(TOOLS_DIR)/staticcheck -version))"
	@command -v gopls >/dev/null || echo "NOTE: $(TOOLS_DIR) is not on your PATH"

# ---------------------------------------------------------------------------
# The loop after touching Go, which is ardanlabs/kronk's and is followed here:
# format, vet, staticcheck, build, test -- every time, on the package that
# changed, fixing the code rather than suppressing the diagnostic.
#
# One package rather than the whole module. In the repository this came from
# that was forced: "staticcheck ./..." there reports about twenty findings in
# packages nobody has touched in months, and every edit arriving under a wall
# of unrelated output is how a check stops being run. Here the module is new
# and clean, so the bound costs nothing -- and the habit is in place before it
# is needed, which is the only time it can be adopted cheaply.

.PHONY: go-check
go-check: ## Format, vet, staticcheck, build and test one package (make go-check PKG=./app/domain/formapp)
	@if [ -z "$(PKG)" ]; then \
		echo "usage: make go-check PKG=./app/domain/formapp" >&2; \
		echo "       the package you changed, not ./... -- see the comment above this target" >&2; \
		exit 2; \
	fi
	@gofmt -s -w $(PKG)
	@$(GO) vet $(PKG)/...
	@if command -v staticcheck >/dev/null; then \
		staticcheck $(PKG)/...; \
	else \
		echo "staticcheck is not installed; run: make dev-tools"; exit 1; \
	fi
	@$(GO) build ./...
	@GO=$(GO) scripts/go-test $(PKG)/...

.PHONY: vet
vet: ## go vet the whole module
	$(GO) vet ./...

.PHONY: fmt
fmt: ## Format all Go sources in place
	$(GO) fmt ./...

.PHONY: fmt-check
fmt-check: ## Fail, naming the files, if anything is not gofmt-clean
	@out=$$(gofmt -s -l .); \
	if [ -n "$$out" ]; then echo "not gofmt-clean:"; echo "$$out"; exit 1; fi

.PHONY: lint
lint: vet fmt-check ## vet + gofmt check

.PHONY: vuln-check
vuln-check: ## Check dependencies against the Go vulnerability database (needs network)
	$(GO) tool govulncheck ./...

.PHONY: test-unit
test-unit: ## Run unit tests, with the race detector
	@# -race, because a server is concurrent in the places that matter, and a
	@# race the detector would have caught is a bug found in production
	@# instead. It needs cgo and a C compiler; CGO_ENABLED=0 is for the release
	@# build, not for tests.
	@#
	@# The passing packages are counted rather than listed, by scripts/go-test.
	@# Everything else is printed untouched. See that script for why the filter
	@# lives there and not in a pipeline somebody adds to each run by hand.
	@GO=$(GO) scripts/go-test -race ./...

.PHONY: test
test: test-unit lint vuln-check ## Full check: unit tests + lint + vulnerability scan

.PHONY: cover
cover: ## Unit tests with a coverage summary
	$(GO) test -cover ./...

.PHONY: tidy
tidy: ## Tidy go.mod
	$(GO) mod tidy

# ---------------------------------------------------------------------------
# Building and running locally.

.PHONY: build
build: ## Build the binary for this machine
	$(GO) build -o $(APP) ./cmd/$(APP)

.PHONY: run
run: build ## Build and run against ./config.toml
	@[ -f config.toml ] || { \
		echo "config.toml does not exist. Start from the documented example:" >&2; \
		echo "    cp config.example.toml config.toml" >&2; \
		exit 1; \
	}
	./$(APP) -config config.toml

.PHONY: release
release: ## Build the static linux/amd64 binary the server runs
	@# CGO off is not a preference. The SQLite driver is pure Go, so the result
	@# is one file with no libc on the server to match against; an ordinary
	@# build links this machine's glibc and dies there with GLIBC_2.xx not
	@# found.
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
		$(GO) build -trimpath -ldflags='-s -w' -o $(APP)-linux-amd64 ./cmd/$(APP)
	@file $(APP)-linux-amd64 | grep -q 'statically linked' \
		&& echo "$(APP)-linux-amd64 is statically linked" \
		|| { echo "NOT statically linked -- the server will refuse to run this" >&2; exit 1; }

# ---------------------------------------------------------------------------
# Secrets. One gitignored file, secrets.env, documented by the tracked
# secrets.env.example -- and two destinations, because continuous deployment
# needs enough to restart a binary and has no business holding a Stripe key.
# scripts/secrets says why at length.

.PHONY: secrets-check
secrets-check: ## Name every missing key in secrets.env, printing no values
	@scripts/secrets check

.PHONY: secrets-push
secrets-push: ## Send the deploy group to GitHub repository secrets (needs gh)
	@scripts/secrets push

.PHONY: secrets-install
secrets-install: ## Write config.toml onto the server from secrets.env (needs ssh)
	@scripts/secrets install

.PHONY: secrets-keygen
secrets-keygen: ## Mint the deploy key, pin the host key, fill both into secrets.env
	@scripts/secrets keygen
