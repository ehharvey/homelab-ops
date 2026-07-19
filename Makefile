.PHONY: build test lint lint-docs fmt tidy clean hooks ship lgtm vendor-incusos docker-build dev validate validate-hardware

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

# Proves docs/'s mermaid diagrams actually parse — a broken one renders as an
# error box on GitHub, which reviewing the source in a diff won't catch (see
# scripts/lint-mermaid.sh and #112).
lint-docs:
	./scripts/lint-mermaid.sh

fmt:
	$(GO) fmt ./...
	goimports -w .

tidy:
	$(GO) mod tidy

clean:
	rm -rf bin/ bootstrap-output/

# Relative path on purpose — see .devcontainer/scripts/4-install-git-hooks.sh.
hooks:
	git config core.hooksPath .githooks

ship:
	./scripts/ship.sh

# make lgtm  /  make lgtm PR=123
lgtm:
	./scripts/lgtm.sh $(PR)

# The unattended subset — exactly what CI runs, same entry point (following
# `make lint-docs`' precedent). --strict makes an unmet prerequisite a failure,
# except the 3.2 GB base image no hosted runner can supply.
#
# Exit 3 ("some checks skipped") is success here: under --strict the only skips
# that survive are ones this command explicitly blessed, so treating 3 as a
# build failure would make a correct run red. run.sh still reports 3 rather than
# 0, because "not everything ran" is worth saying out loud — the Makefile
# decides what that means for a gate, the harness only reports it.
validate:
	@./scripts/validate/run.sh --group none,compose --strict --allow-skip base-image; \
	rc=$$?; [ $$rc -eq 0 ] || [ $$rc -eq 3 ] || exit $$rc

# Needs the Incus remote, home-lan, INCUSOS_BASE_IMAGE and flasher-tool.
# Serial by necessity: these share the home-lan bridge.
validate-hardware:
	./scripts/validate/run.sh --group incus,incus-vm

vendor-incusos:
	./scripts/vendor-incusos.sh

docker-build:
	docker build -t homelab-ops-web .

dev:
	docker compose up --build

