GO      ?= go
VERSION ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
LDFLAGS  = -X main.version=$(VERSION)

.PHONY: check fmt-check vet test build db-up db-down migrate-lab migrate-kbase clean

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

db-up:
	docker compose up -d --wait postgres

db-down:
	docker compose down

migrate-lab:
	$(GO) run ./cmd/migrate -stream lab up

migrate-kbase:
	$(GO) run ./cmd/migrate -stream kbase up

clean:
	rm -rf bin
