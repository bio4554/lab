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

db-up:
	docker compose up -d --wait postgres

db-down:
	docker compose down

migrate-lab:
	$(GO) run ./cmd/migrate -stream lab up

migrate-kbase:
	$(GO) run ./cmd/migrate -stream kbase up

# startd: everything a fresh clone needs to run the daemon — Postgres
# up, lab schema migrated, labd built and started. Credentials for
# env-passthrough agents are sourced from ~/.lab/demo.env when present
# (keep that file outside the repo; it holds ANTHROPIC_API_KEY or
# CLAUDE_CODE_OAUTH_TOKEN).
startd: db-up migrate-lab
	$(GO) build -ldflags '$(LDFLAGS)' -o bin/labd ./cmd/labd
	@if [ -f "$$HOME/.lab/demo.env" ]; then \
		echo "sourcing credentials from ~/.lab/demo.env"; \
		set -a; . "$$HOME/.lab/demo.env"; set +a; exec ./bin/labd; \
	else \
		echo "note: no ~/.lab/demo.env — agents with env-passthrough credentials need ANTHROPIC_API_KEY or CLAUDE_CODE_OAUTH_TOKEN in this environment"; \
		exec ./bin/labd; \
	fi

# startc: build and start the TUI client (labd must be running — see
# startd).
startc:
	$(GO) build -ldflags '$(LDFLAGS)' -o bin/lab ./cmd/lab
	@exec ./bin/lab

clean:
	rm -rf bin
