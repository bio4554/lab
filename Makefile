GO      ?= go
VERSION ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
LDFLAGS  = -X main.version=$(VERSION)

.PHONY: check fmt-check vet test e2e build db-up db-down migrate-lab migrate-kbase clean startd startc

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

# e2e: the deterministic end-to-end suite — real labd + kbased +
# Postgres + Docker containers, with a scripted claude stand-in (no
# Anthropic credential needed). Requires Docker and the compose
# Postgres (`make db-up`); skips gracefully when either is absent.
# First run builds the base agent image (network for apt); later runs
# hit the image cache.
e2e:
	$(GO) test -tags e2e -count=1 -timeout 30m -v ./e2e

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

# startd: everything a fresh clone needs to run the daemons — Postgres
# up, both schemas migrated, kbased + labd built and started together
# (kbased in the background, labd in the foreground; both stop on
# Ctrl-C). Anthropic credentials are NOT read from the environment:
# onboard them once with `labctl cred add` (see README).
#
# The kbased admin token the two daemons share comes from, in order:
# LAB_KBASED_ADMIN_TOKEN in the environment or ~/.lab/demo.env (both
# kept outside the repo), else ~/.lab/kbased.token — generated 0600 on
# first run so kbase wiring is zero-config for dev.
startd: db-up migrate-lab migrate-kbase
	$(GO) build -ldflags '$(LDFLAGS)' -o bin/labd ./cmd/labd
	$(GO) build -ldflags '$(LDFLAGS)' -o bin/kbased ./cmd/kbased
	@set -e; \
	if [ -f "$$HOME/.lab/demo.env" ]; then \
		echo "sourcing environment from ~/.lab/demo.env"; \
		set -a; . "$$HOME/.lab/demo.env"; set +a; \
	fi; \
	if [ -z "$$LAB_KBASED_ADMIN_TOKEN" ]; then \
		mkdir -p "$$HOME/.lab"; \
		if [ ! -f "$$HOME/.lab/kbased.token" ]; then \
			umask 077; openssl rand -hex 32 > "$$HOME/.lab/kbased.token"; \
			echo "generated kbased admin token at ~/.lab/kbased.token"; \
		fi; \
		LAB_KBASED_ADMIN_TOKEN="$$(cat "$$HOME/.lab/kbased.token")"; \
		export LAB_KBASED_ADMIN_TOKEN; \
	fi; \
	./bin/kbased & kpid=$$!; \
	trap 'kill -TERM $$kpid 2>/dev/null' INT TERM EXIT; \
	./bin/labd

# startc: build and start the TUI client (labd must be running — see
# startd).
startc:
	$(GO) build -ldflags '$(LDFLAGS)' -o bin/lab ./cmd/lab
	@exec ./bin/lab

clean:
	rm -rf bin
