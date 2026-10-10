.PHONY: build test lint lint-docs fmt tidy clean hooks ship lgtm wt wt-list wt-gc vendor-incusos docker-build dev validate validate-hardware incusos-base

GO ?= go
BOOTSTRAP_BIN := bin/bootstrap
WEB_BIN := bin/web
AGENT_BIN := bin/agent
LINT_IMAGE := golangci/golangci-lint:v2.12.2

build:
	$(GO) build -o $(BOOTSTRAP_BIN) ./cmd/bootstrap
	$(GO) build -o $(WEB_BIN) ./cmd/web
	$(GO) build -o $(AGENT_BIN) ./cmd/agent

test:
	$(GO) test ./... -race -cover

# One lint at a time across every worktree of this repo (#282): each run is a
# fresh container that re-downloads modules and peaks near 3 GiB, so N runs at
# once each take ~N times as long and push the host into swap. The lock lives
# in the shared git dir, beside validate.lock. A killed make frees it at once,
# though a SIGKILLed `docker run` leaves its container running to the end.
# The first, non-blocking try exits 75 only when the lock is held, so the wait
# gets announced. No flock (macOS) or no git checkout: run unlocked.
LINT_CMD := docker run --rm -v $(CURDIR):/app -w /app $(LINT_IMAGE) golangci-lint run ./...
LINT_LOCK := $(shell git rev-parse --path-format=absolute --git-common-dir 2>/dev/null)/lint.lock
LINT_LOCKED := $(and $(shell command -v flock 2>/dev/null),$(filter-out /lint.lock,$(LINT_LOCK)))

lint:
ifeq ($(LINT_LOCKED),)
	$(LINT_CMD)
else
	@echo 'flock $(LINT_LOCK) $(LINT_CMD)'
	@flock -n -E 75 "$(LINT_LOCK)" $(LINT_CMD); rc=$$?; [ $$rc -eq 75 ] || exit $$rc; \
	echo "make lint: waiting for another make lint to release $(LINT_LOCK)"; \
	flock "$(LINT_LOCK)" $(LINT_CMD)
endif

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

# One worktree per issue, outside the operator's checkout (scripts/worktree.sh,
# #252). `make wt N=123` prints the path; @ keeps it the only stdout line.
wt:
	@./scripts/worktree.sh new $(N)

wt-list:
	@./scripts/worktree.sh list

wt-gc:
	@./scripts/worktree.sh gc

# The unattended subset, and CI's intended entry point when a validate workflow
# exists — there isn't one yet, so today this only runs by hand. Same
# entry-point shape as `make lint-docs`, following its precedent. --strict makes
# an unmet prerequisite a failure, except the 3.2 GB base image no hosted runner
# can supply. Where `make incusos-base` has cached the pinned image, those
# checks run instead of skipping.
#
# Exit 3 ("some checks skipped") is success here: under --strict the only skips
# that survive are ones this command explicitly blessed, so treating 3 as a
# build failure would make a correct run red. run.sh still reports 3 rather than
# 0, because "not everything ran" is worth saying out loud — the Makefile
# decides what that means for a gate, the harness only reports it.
validate:
	@./scripts/validate/run.sh --group none,compose --strict --allow-skip base-image; \
	rc=$$?; [ $$rc -eq 0 ] || [ $$rc -eq 3 ] || exit $$rc

# Needs the Incus remote, home-lan, flasher-tool and the IncusOS base image.
# INCUSOS_BASE_IMAGE defaults to the pinned version `make incusos-base` cached;
# export it to use another image. Serial by necessity: these share the home-lan
# bridge.
validate-hardware:
	./scripts/validate/run.sh --group incus,incus-vm

# Fetch, verify and cache the IncusOS base image pinned in
# scripts/incusos-base.version (~610 MB down, 3.2 GB on disk) under the main
# checkout's bootstrap-output/incusos/, shared by every worktree. A no-op once
# cached. Prints the `export INCUSOS_BASE_IMAGE=...` line on stdout; the
# validate scripts find the pinned image without it (#296).
incusos-base:
	@./scripts/fetch-incusos-base.sh

vendor-incusos:
	./scripts/vendor-incusos.sh

docker-build:
	docker build -t homelab-ops-web .

dev:
	docker compose up --build

