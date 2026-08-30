BINARY := yt-dlp-manager

# Version stamping, mirroring what .github/workflows/release.yml does for a
# tagged build. Without this a locally built binary reports the compile-time
# defaults from internal/app (dev / unknown / unknown), which reads like a bug
# in `--help` and on the web UI's About page.
#
# Overridable: `make build VERSION=v1.2.3`. The fallbacks cover a checkout with
# no tags (or no git at all), where `git describe` has nothing to report.
# Sanitised inside the $(shell ...) on purpose. Git ref names may legally
# contain shell metacharacters — `git check-ref-format` accepts `v1.0$(id)`,
# `v1.0;cmd` and backticks; only whitespace is barred — and make pastes a
# variable straight into the recipe text that /bin/sh then evaluates. Cloning an
# untrusted repository and running `make build` was therefore enough to execute
# the tag. Filtering here means the metacharacters never reach a recipe at all;
# the quoting at each use site below is a second line of defence.
VERSION ?= $(shell v=$$(git describe --tags --always --dirty 2>/dev/null | tr -cd 'A-Za-z0-9._-'); echo "$${v:-dev}")
COMMIT  ?= $(shell c=$$(git rev-parse HEAD 2>/dev/null | tr -cd '0-9a-f'); echo "$${c:-unknown}")
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

# Deliberately no -s -w here, unlike the release build: stripping the symbol
# table and DWARF saves a couple of MB nobody cares about locally and breaks
# delve. Release binaries are stripped; development binaries stay debuggable.
LDFLAGS := -X yt-dlp-manager/internal/app.Version=$(VERSION) \
           -X yt-dlp-manager/internal/app.Commit=$(COMMIT) \
           -X yt-dlp-manager/internal/app.Date=$(DATE)

.PHONY: all build test race vet fmt clean install frontend e2e image \
        up down purge up-prod down-prod pull-prod

all: build

build:
	go build -trimpath -ldflags="$(LDFLAGS)" -o bin/$(BINARY) ./cmd/$(BINARY)

# Rebuild the embedded web UI (requires Node >= 22). The committed snapshot
# in internal/webui/static keeps plain `go build` working without Node.
frontend:
	cd internal/webui/frontend && npm ci && npm run build

test:
	go test -count=1 ./...

# Real-browser tests against the embedded UI. The binary embeds the bundle at
# compile time, so the frontend is rebuilt first and the binary after it —
# skipping either silently tests the previous UI. Needs Node and, once,
# `npx --prefix e2e playwright install chromium` once for the browser itself;
# package dependencies are installed reproducibly by the target.
e2e: frontend
	$(MAKE) build
	cd e2e && npm ci && npm run test

race:
	go test -race -count=1 ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

clean:
	rm -rf bin/

install: build
	install -Dm755 bin/$(BINARY) ~/.local/bin/$(BINARY)

# The build args matter: .dockerignore keeps .git out of the build context, so
# the Dockerfile cannot derive them itself and falls back to dev/unknown/unknown,
# which is what the web UI's About page then reports.
# Bring the stack up with a properly stamped image. `docker compose up --build`
# on its own cannot do this: compose has no way to shell out to git, so its
# build args fall back to dev/unknown/unknown and the About page says so.
up:
	APP_VERSION='$(VERSION)' APP_COMMIT='$(COMMIT)' APP_DATE='$(DATE)' \
	  docker compose up -d --build

down:
	docker compose down

# --- Published-image stack (compose-prod.yaml) ------------------------------
# Pulls ghcr.io/ih8d8/yt-dlp-manager and builds nothing, so none of the version
# stamping above applies: the image already carries the stamps from the release
# that produced it.
#
# Both compose files declare the same container_name and the same config/state
# volumes, and they live in the same directory, so Compose gives them one
# project. Run one stack or the other, never both at once — and note that
# `make purge` removes the volumes for whichever one is in use, because they
# are the same volumes.
PROD := docker compose -f compose-prod.yaml

up-prod:
	$(PROD) up -d

down-prod:
	$(PROD) down

# Refresh to the newest published image without changing anything else. The
# service sets pull_policy: always, so `up-prod` already pulls; this is for
# pulling ahead of time, or re-pulling a moving tag like :latest or :0.1.
pull-prod:
	$(PROD) pull

# Same as `down`, plus `-v`: the named volumes go too. That is /config and
# /state — the administrator password verifier, the managed yt-dlp config, the
# queue, and the whole download history. First-run setup happens again on the
# next `make up`.
#
# Downloaded MEDIA survives. ~/Downloads/yt-dlp is a bind mount, and
# `docker compose down -v` only removes named volumes, never bind sources.
#
# Prompts first because none of it is recoverable. For a script, skip the
# prompt with `make purge CONFIRM=yes`.
purge:
	@if [ "$(CONFIRM)" != "yes" ]; then \
	  printf 'Delete the config and state volumes?\n'; \
	  printf '  lost: admin password, yt-dlp config, queue and download history\n'; \
	  printf '  kept: downloaded files in the /downloads bind mount\n'; \
	  printf 'Type "yes" to continue: '; \
	  read -r ans; \
	  [ "$$ans" = "yes" ] || { echo "Aborted."; exit 1; }; \
	fi
	docker compose down -v

image:
	docker build -t yt-dlp-manager:latest \
	  --build-arg APP_VERSION='$(VERSION)' \
	  --build-arg APP_COMMIT='$(COMMIT)' \
	  --build-arg APP_DATE='$(DATE)' .
