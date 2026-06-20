.PHONY: build test lint fmt tidy clean vendor-incusos

GO ?= go
BOOTSTRAP_BIN := bin/bootstrap
LINT_IMAGE := golangci/golangci-lint:v2.12.2

build:
	$(GO) build -o $(BOOTSTRAP_BIN) ./cmd/bootstrap

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
