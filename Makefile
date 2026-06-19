.PHONY: build test lint fmt tidy clean

GO ?= go
BOOTSTRAP_BIN := bin/bootstrap

build:
	$(GO) build -o $(BOOTSTRAP_BIN) ./cmd/bootstrap

test:
	$(GO) test ./... -race -cover

lint:
	golangci-lint run ./...

fmt:
	$(GO) fmt ./...
	goimports -w .

tidy:
	$(GO) mod tidy

clean:
	rm -rf bin/ bootstrap-output/
