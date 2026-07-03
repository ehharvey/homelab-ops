.PHONY: build test lint fmt tidy clean vendor-incusos docker-build dev validate-sprint-3

GO ?= go
BOOTSTRAP_BIN := bin/bootstrap
WEB_BIN := bin/web
LINT_IMAGE := golangci/golangci-lint:v2.12.2

build:
	$(GO) build -o $(BOOTSTRAP_BIN) ./cmd/bootstrap
	$(GO) build -o $(WEB_BIN) ./cmd/web

test:
	$(GO) test ./... -race -cover

lint:
	docker run --rm -v $(CURDIR):/app -w /app $(LINT_IMAGE) golangci-lint run ./...

fmt:
	$(GO) fmt ./...
	goimports -w .

tidy:
	$(GO) mod tidy

clean:
	rm -rf bin/ bootstrap-output/

vendor-incusos:
	./scripts/vendor-incusos.sh

docker-build:
	docker build -t homelab-ops-web .

dev:
	docker compose up --build

validate-sprint-3:
	./scripts/validate-sprint-3.sh
