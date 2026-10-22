# SPDX-License-Identifier: Apache-2.0

# Modules in this repository. The root module must stay free of requirements (NFR-7).
MODULES := . surecachepg promobs examples/groups

# Pinned tool versions, installed into ./bin by `make tools`.
GOLANGCI_LINT_VERSION := v2.15.0
GREMLINS_VERSION      := v0.6.0
BENCHSTAT_VERSION     := v0.0.0-20261009192801-be2c69fb417e
GOVULNCHECK_VERSION   := v1.9.0

BIN := $(CURDIR)/bin
tool = $(if $(wildcard $(BIN)/$(1)),$(BIN)/$(1),$(1))
GOLANGCI_LINT := $(call tool,golangci-lint)
GREMLINS      := $(call tool,gremlins)
GOVULNCHECK   := $(call tool,govulncheck)

# Coverage gate (D7): total at least 95%, and 100% on the files below.
COVER_MIN   := 95
COVER_FULL  := read.go,mutate.go,refresh.go,rebuild.go,outage.go
COVER_FLAGS :=

# Fuzzing and the long model-checker run (nightly).
FUZZ_TARGETS := FuzzBulkInstallContract FuzzRefreshSequence
FUZZTIME     ?= 5m
MODEL_BUDGET ?= 2s
MODEL_SEEDS  ?= 50

# Local PostgreSQL from compose.yaml.
PG_DSN ?= postgres://surecache:surecache@localhost:55432/postgres?sslmode=disable

.PHONY: all lint test alloc cover bench mutate fuzz model pgtest vuln tools deps-check

all: lint test

lint:
	@for m in $(MODULES); do \
		echo "==> lint $$m"; \
		(cd $$m && $(GOLANGCI_LINT) run ./...) || exit 1; \
	done

test:
	@for m in $(MODULES); do \
		echo "==> test $$m"; \
		(cd $$m && go test -race -shuffle=on ./...) || exit 1; \
	done

# Allocation tests (NFR-3) skip under -race, so they run separately without it.
alloc:
	go test -count=1 -run 'Allocs|ZeroAllocs|OneAlloc' -v .

cover:
	go test -race -coverprofile=coverage.out ./...
	go run ./scripts/covercheck -profile coverage.out -min $(COVER_MIN) -full $(COVER_FULL) $(COVER_FLAGS)

bench:
	go test -run '^$$' -bench . -benchmem -count 10 ./...

mutate:
	GOWORK=off $(GREMLINS) unleash --config .gremlins.yaml --threshold-efficacy 90 .

fuzz:
	@for t in $(FUZZ_TARGETS); do \
		echo "==> fuzz $$t"; \
		go test -run '^$$' -fuzz "^$$t$$" -fuzztime $(FUZZTIME) . || exit 1; \
	done

model:
	go test -race -count=1 -timeout 60m -run '^TestModel_' . -args -model.budget=$(MODEL_BUDGET) -model.seeds=$(MODEL_SEEDS)

pgtest:
	docker compose up -d --wait
	cd surecachepg && SURECACHE_PG_DSN='$(PG_DSN)' go test -race ./...
	cd examples/groups && SURECACHE_PG_DSN='$(PG_DSN)' go test -race ./...

vuln:
	@for m in $(MODULES); do \
		echo "==> govulncheck $$m"; \
		(cd $$m && $(GOVULNCHECK) ./...) || exit 1; \
	done

# NFR-7: the root module resolves no dependency outside the workspace.
deps-check:
	@out="$$(GOWORK=off go list -m all)"; \
	if [ "$$out" != "github.com/nobelk/surecache" ]; then \
		echo "root module has dependencies:"; echo "$$out"; exit 1; \
	fi

tools:
	GOBIN=$(BIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	GOBIN=$(BIN) go install github.com/go-gremlins/gremlins/cmd/gremlins@$(GREMLINS_VERSION)
	GOBIN=$(BIN) go install golang.org/x/perf/cmd/benchstat@$(BENCHSTAT_VERSION)
	GOBIN=$(BIN) go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
