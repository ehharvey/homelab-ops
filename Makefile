.PHONY: build test lint lint-sh lint-docs fmt tidy clean hooks ship lgtm ready wt wt-list wt-gc vendor-incusos docker-build dev validate validate-hardware incusos-base appliance-image appliance-check appliance-lock

GO ?= go
BOOTSTRAP_BIN := bin/bootstrap
WEB_BIN := bin/web
AGENT_BIN := bin/agent
# The golangci-lint version lives only in .golangci-lint-version (#329): this
# image tag, and so `make lint` and `make fmt`, and CI's lint job (through
# golangci-lint-action's version-file) all read it. To bump, edit that file.
LINT_IMAGE := golangci/golangci-lint:$(shell cat .golangci-lint-version)
# Likewise the shellcheck version lives only in .shellcheck-version (#418):
# this image tag, which `make lint-sh` and so `make lint` and CI's lint job run,
# reads it, and scripts/lint-sh.sh builds the same tag when run on its own.
SHELLCHECK_IMAGE := koalaman/shellcheck:$(shell cat .shellcheck-version)

build:
	$(GO) build -o $(BOOTSTRAP_BIN) ./cmd/bootstrap
	$(GO) build -o $(WEB_BIN) ./cmd/web
	$(GO) build -o $(AGENT_BIN) ./cmd/agent

test:
	$(GO) test ./... -race -cover

# One lint at a time across every worktree of this repo (#282): each run is a
# fresh container that re-downloads modules and peaks near 3 GiB, so N runs at
# once each take ~N times as long and push the host into swap. The lock lives
# in the shared git dir, beside validate.lock. A killed make frees it at once.
# The first, non-blocking try exits 75 only when the lock is held, so the wait
# gets announced. No flock (macOS) or no git checkout: run unlocked, unlabelled.
# -o closes the lock fd before running the command (#301), so only flock holds
# the lock: a child that outlives the lint (a hung docker credential helper,
# #300) can't keep it held. The tradeoff: the lock lives only as long as the
# flock process, so killing flock alone frees it while docker may still run.
# A SIGKILLed `docker run` client likewise leaves its container running. So the
# locked run labels its container $(LINT_LABEL)=<the lock path> and, under the
# lock and before starting its own, removes every running container carrying
# that label (#292): with the lock held, any such container is an orphan.
# Worktrees share the lock path, so they reap each other's orphans; a separate
# clone has its own lock and label value, and is never touched. LINT_LOCKED_CMD
# takes the lock path as $1 and the checkout as $2, so spaces survive.
LINT_CMD := docker run --rm -v $(CURDIR):/app -w /app $(LINT_IMAGE) golangci-lint run ./...
LINT_LOCK := $(shell git rev-parse --path-format=absolute --git-common-dir 2>/dev/null)/lint.lock
LINT_LOCKED := $(and $(shell command -v flock 2>/dev/null),$(filter-out /lint.lock,$(LINT_LOCK)))
LINT_LABEL := homelab.lint.lock
LINT_LOCKED_CMD := sh -c 'docker ps -q --filter "label=$(LINT_LABEL)=$$1" | while read -r id; do \
	  echo "make lint: reaping orphaned lint container $$(docker inspect -f "{{.Name}}" "$$id" | tr -d /) ($$id), labelled $(LINT_LABEL)=$$1"; \
	  docker rm -f "$$id" >/dev/null; \
	done; \
	exec docker run --rm --label "$(LINT_LABEL)=$$1" -v "$$2":/app -w /app $(LINT_IMAGE) golangci-lint run ./...' \
	make-lint "$(LINT_LOCK)" "$(CURDIR)"

lint: lint-sh
ifeq ($(LINT_LOCKED),)
	$(LINT_CMD)
else
	@echo 'flock -o $(LINT_LOCK) docker run --label $(LINT_LABEL)=$(LINT_LOCK) ... golangci-lint run ./... (after reaping that label)'
	@flock -n -E 75 -o "$(LINT_LOCK)" $(LINT_LOCKED_CMD); rc=$$?; [ $$rc -eq 75 ] || exit $$rc; \
	echo "make lint: waiting for another make lint to release $(LINT_LOCK)"; \
	flock -o "$(LINT_LOCK)" $(LINT_LOCKED_CMD)
endif

# shellcheck over every tracked shell script (#418): each *.sh, plus extensionless
# files with an sh or bash shebang, listed by git so .claude/worktrees/ is never
# scanned. `make lint` runs it first. It doesn't take lint.lock: one small
# container that reads a few dozen files is cheap beside a running lint.
lint-sh:
	SHELLCHECK_IMAGE=$(SHELLCHECK_IMAGE) ./scripts/lint-sh.sh

# Proves docs/'s mermaid diagrams actually parse — a broken one renders as an
# error box on GitHub, which reviewing the source in a diff won't catch (see
# scripts/lint-mermaid.sh and #112).
lint-docs:
	./scripts/lint-mermaid.sh

# Formats with the same golangci-lint, and so the same gofmt and goimports, that
# `make lint` checks against (#329): the formatters enabled in .golangci.yml.
# `golangci-lint fmt` walks the tree from here, skipping dot directories (so
# the worktrees under .claude/worktrees/) by itself; .golangci.yml excludes the
# upstream submodule in third_party/. It doesn't take lint.lock: it loads no
# packages or modules, so it is cheap beside a running lint.
fmt:
	docker run --rm -v $(CURDIR):/app -w /app $(LINT_IMAGE) golangci-lint fmt

tidy:
	$(GO) mod tidy

clean:
	rm -rf bin/ bootstrap-output/

# Relative path on purpose — see .devcontainer/scripts/4-install-git-hooks.sh.
# commit.cleanup=scissors keeps `## Plan` / `## Test plan` through an amend
# (Development Conventions § PR body format, #334).
hooks:
	git config core.hooksPath .githooks
	git config commit.cleanup scissors

ship:
	./scripts/ship.sh

# make lgtm  /  make lgtm PR=123
lgtm:
	./scripts/lgtm.sh $(PR)

# make ready N=310  /  make ready N="310 311": add Ready and drop proposed in
# one edit, refusing an issue with an open blocker (scripts/ready.sh, #385).
# Each word is quoted, so the shell doesn't read N=#310 as a comment.
ready:
	./scripts/ready.sh $(foreach n,$(N),'$(n)')

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

# The web app appliance's NixOS image (#243, deploy/nixos/, docs/Deployment.md
# § The appliance). Nix runs in a pinned nixos/nix container, the way `make
# lint` runs golangci-lint, so the devcontainer needs no Nix install. The
# container's /nix is a named volume, so the store and its downloads survive
# between runs; the volume is named for the Nix version, so a bumped image
# starts a fresh one. The flake is passed as path:/src/deploy?dir=nixos, so
# deploy/compose.yaml is inside its source tree. Without a usable /dev/kvm
# (this devcontainer has none) QEMU falls back to TCG: the image build and the
# nixosTest still run, only slower. "kvm" is declared as a system feature
# either way, because the derivations that boot a VM require it.
#
#   make appliance-image  bootstrap-output/appliance/homelab-ops-appliance.img
#   make appliance-check  nix flake check: evaluates the flake, runs the nixosTest
#   make appliance-lock   nix flake update: re-pins deploy/nixos/flake.lock
NIX_IMAGE := nixos/nix:2.35.3@sha256:ff480dbb23511aa1c10710aa068ceea5b93fed9bfce5f5379f3a6b07282cbc7b
NIX_STORE_VOLUME := homelab-ops-nix-2.35.3
NIX_FLAKE := path:/src/deploy?dir=nixos
NIX := nix --extra-experimental-features "nix-command flakes" \
	--option system-features "kvm nixos-test big-parallel benchmark" --option warn-dirty false
APPLIANCE_OUT := bootstrap-output/appliance
APPLIANCE_IMG := homelab-ops-appliance.img
# Recursive (=), so only the appliance recipes that use them run these probes.
HOST_IDS = $(shell id -u):$(shell id -g)
KVM_DEVICE = $(shell [ -r /dev/kvm ] && [ -w /dev/kvm ] && echo --device /dev/kvm)
# $(call nix_run,<deploy mount mode>) — the container, deploy/ mounted at /src/deploy.
nix_run = docker run --rm $(KVM_DEVICE) -v $(NIX_STORE_VOLUME):/nix -v "$(CURDIR)/deploy:/src/deploy:$(1)"

appliance-image:
	mkdir -p $(APPLIANCE_OUT)
	$(call nix_run,ro) -v "$(CURDIR)/$(APPLIANCE_OUT):/out" $(NIX_IMAGE) sh -c '\
	  $(NIX) build -L --out-link /tmp/result "$(NIX_FLAKE)#raw-efi" && \
	  cp -L /tmp/result/$(APPLIANCE_IMG) /out/$(APPLIANCE_IMG).tmp && \
	  chown $(HOST_IDS) /out/$(APPLIANCE_IMG).tmp && chmod 0644 /out/$(APPLIANCE_IMG).tmp && \
	  mv /out/$(APPLIANCE_IMG).tmp /out/$(APPLIANCE_IMG)'
	@echo "$(APPLIANCE_OUT)/$(APPLIANCE_IMG)"

appliance-check:
	$(call nix_run,ro) $(NIX_IMAGE) $(NIX) flake check -L "$(NIX_FLAKE)"

appliance-lock:
	$(call nix_run,rw) $(NIX_IMAGE) sh -c '\
	  $(NIX) flake update --flake "$(NIX_FLAKE)" && chown $(HOST_IDS) /src/deploy/nixos/flake.lock'

