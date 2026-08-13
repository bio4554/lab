GO      ?= go
VERSION ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
LDFLAGS  = -X main.version=$(VERSION)

.PHONY: check fmt-check vet test build db-up db-down migrate-lab migrate-kbase clean startd startc

check: fmt-check vet
	$(GO) build ./...
	$(GO) test ./...

fmt-check:
	@fmt_out="$$(gofmt -l .)"; \
	if [ -n "$$fmt_out" ]; then \
		echo "gofmt needed on:"; echo "$$fmt_out"; exit 1; \
	fi

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

build:
	$(GO) build -ldflags '$(LDFLAGS)' -o bin/labd ./cmd/labd
	$(GO) build -ldflags '$(LDFLAGS)' -o bin/kbased ./cmd/kbased
	$(GO) build -ldflags '$(LDFLAGS)' -o bin/kbase ./cmd/kbase
	$(GO) build -ldflags '$(LDFLAGS)' -o bin/lab ./cmd/lab
	$(GO) build -ldflags '$(LDFLAGS)' -o bin/labctl ./cmd/labctl

db-up:
	docker compose up -d --wait postgres

db-down:
	docker compose down

migrate-lab:
	$(GO) run ./cmd/migrate -stream lab up

migrate-kbase:
	$(GO) run ./cmd/migrate -stream kbase up

# startd: everything a fresh clone needs to run the daemon — Postgres
# up, both schemas migrated, labd built and started. Anthropic
# credentials are NOT read from the environment: onboard them once with
# `labctl cred add` (see README). ~/.lab/demo.env, when present, is
# sourced for optional environment like LAB_KBASED_ADMIN_TOKEN (keep
# that file outside the repo).
startd: db-up migrate-lab migrate-kbase
	$(GO) build -ldflags '$(LDFLAGS)' -o bin/labd ./cmd/labd
	@if [ -f "$$HOME/.lab/demo.env" ]; then \
		echo "sourcing environment from ~/.lab/demo.env"; \
		set -a; . "$$HOME/.lab/demo.env"; set +a; exec ./bin/labd; \
	else \
		exec ./bin/labd; \
	fi

# startc: build and start the TUI client (labd must be running — see
# startd).
startc:
	$(GO) build -ldflags '$(LDFLAGS)' -o bin/lab ./cmd/lab
	@exec ./bin/lab

clean:
	rm -rf bin
