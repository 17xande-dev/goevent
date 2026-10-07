COMPOSE ?= docker compose
export LOCAL_UID := $(shell id -u)
export LOCAL_GID := $(shell id -g)
# Port 5433 rather than 5432, so this stack runs beside gostore's (or any other
# local Postgres) without a fight over the port.
TEST_DATABASE_URL ?= postgres://goevent:goevent@localhost:5433/goevent?sslmode=disable

# There are no admin credentials to set: the first administrator is claimed at
# /admin/setup with the one-time token `make run` prints on a fresh database.
# SETUP_TOKEN, if exported, supplies that token instead of having one generated.
# PayFast's own published sandbox credentials, matching compose.yaml, so that
# `make run` and `make migrate` work on a clean checkout with no .env at all.
# They are in PayFast's documentation and take no real money — but
# PAYFAST_SANDBOX must stay true for that to remain the case.
PAYFAST_MERCHANT_ID ?= 10000100
PAYFAST_MERCHANT_KEY ?= 46f0cd694581a
PAYFAST_PASSPHRASE ?= jt7NOE43FZPn
PAYFAST_SANDBOX ?= true
# SnapScan is off unless a snap code is exported, and deliberately has no default:
# it has no sandbox, so any value here takes real money. Export all three to try
# the QR hand-over locally — and note that the webhook cannot reach a laptop
# without a tunnel, since SnapScan's support configures that address on the
# account rather than reading it from each payment.
SNAPSCAN_SNAP_CODE ?=
SNAPSCAN_API_KEY ?=
SNAPSCAN_WEBHOOK_AUTH_KEY ?=
SNAPSCAN_VALIDATION_KEY ?=
# Recipes using DEV_ENV are prefixed with @ so a real PAYFAST_PASSPHRASE or
# SETUP_TOKEN is not echoed into a terminal or a CI log.
DEV_ENV = DATABASE_URL="$(TEST_DATABASE_URL)" \
	SETUP_TOKEN="$(SETUP_TOKEN)" \
	PAYFAST_MERCHANT_ID="$(PAYFAST_MERCHANT_ID)" \
	PAYFAST_MERCHANT_KEY="$(PAYFAST_MERCHANT_KEY)" \
	PAYFAST_PASSPHRASE="$(PAYFAST_PASSPHRASE)" \
	PAYFAST_SANDBOX="$(PAYFAST_SANDBOX)" \
	SNAPSCAN_SNAP_CODE="$(SNAPSCAN_SNAP_CODE)" \
	SNAPSCAN_API_KEY="$(SNAPSCAN_API_KEY)" \
	SNAPSCAN_WEBHOOK_AUTH_KEY="$(SNAPSCAN_WEBHOOK_AUTH_KEY)" \
	SNAPSCAN_VALIDATION_KEY="$(SNAPSCAN_VALIDATION_KEY)" \
	SECRET_KEY="$(SECRET_KEY)" \
	IMAGE_DIR="$(IMAGE_DIR)" \
	SMTP_HOST="$(SMTP_HOST)" \
	SMTP_PORT="$(SMTP_PORT)" \
	SMTP_TLS="$(SMTP_TLS)" \
	EMAIL_FROM="$(EMAIL_FROM)" \
	NOTIFY_EMAIL="$(NOTIFY_EMAIL)"

# Images and mail are required, so `make run` has to supply both. A directory
# under .local keeps uploaded pictures out of the working tree; mailpit is the
# compose relay, which `make run` starts alongside postgres.
IMAGE_DIR ?= .local/images
# Public development key; deployments must generate and back up their own.
SECRET_KEY ?= abababababababababababababababababababababababababababababababab
SMTP_HOST ?= localhost
SMTP_PORT ?= 1026
SMTP_TLS ?= none
EMAIL_FROM ?= events@goevent.example
NOTIFY_EMAIL ?= organiser@goevent.example

# `make run` serves ./theme and re-reads it on every request, so a theme edit
# needs a page refresh rather than a restart. Never on in a deployment.
THEME_RELOAD ?= true

# sqlc generates the row structs and scan code for the stores. It is pinned here
# rather than as a `go tool` directive in go.mod, so that go.mod keeps stating the
# dependencies of the *binary* — sqlc adds about forty indirect modules and never
# links into it. See "Dependencies" in docs/development.md.
SQLC_VERSION ?= v1.31.1
SQLC ?= sqlc

# The published image. There is no CI publishing this — see "publish" below —
# so TAG defaults to a real version tag when HEAD has one (git tag v1.2.3) and
# to the short commit sha otherwise, which `publish` then refuses: goevent is
# published for other people to run, so the tag is the version contract, not a
# build's provenance.
IMAGE ?= ghcr.io/17xande-dev/goevent
GIT_SHA := $(shell git rev-parse --short HEAD 2>/dev/null)
TAG ?= $(shell git describe --tags --exact-match 2>/dev/null || echo $(GIT_SHA))

# The binary is CGO_ENABLED=0 (see Dockerfile), so no architecture is pinned for
# correctness — linux/amd64 is just what the servers it runs on are.
PLATFORM ?= linux/amd64

.PHONY: up local-dirs down logs run build test vet fmt tidy psql migrate migrate-status hashpw \
	check-config sqlc sqlc-check sqlc-install image check-clean check-tagged publish

## up: build and start the whole local stack
up: local-dirs
	$(COMPOSE) up --build -d
	@echo "server   http://localhost:8081/healthz"
	@echo "mailpit  http://localhost:8026"
	@echo "files    .local/images"

local-dirs:
	mkdir -p .local/images

## down: stop the stack (add ARGS=-v to also delete data volumes)
down:
	$(COMPOSE) down $(ARGS)

logs:
	$(COMPOSE) logs -f server

## run: run the server on the host against the compose Postgres
# Themed from ./theme with reloading on, matching the compose stack: edit a file
# there and refresh, no restart. THEME_RELOAD=false for the read-once behaviour a
# deployment has.
run: local-dirs
	$(COMPOSE) up -d --wait postgres mailpit
	@$(DEV_ENV) TEMPLATE_DIR=theme/templates STATIC_DIR=theme/static \
		THEME_RELOAD="$(THEME_RELOAD)" go run .

## migrate: apply pending migrations without starting the server
migrate:
	$(COMPOSE) up -d --wait postgres
	@$(DEV_ENV) go run . -migrate

## migrate-status: show which migrations have been applied
migrate-status:
	@$(DEV_ENV) go run . -migrate-status

## check-config: validate the full server configuration without starting anything
check-config:
	@$(DEV_ENV) go run . -check-config

## hashpw: read a password from the terminal and print an argon2id hash
# A lockout-recovery path, not part of setup: the first administrator is claimed at
# /admin/setup. See cmd/hashpw for the UPDATE this hash goes into, and the DELETE
# that must go with it.
#
# The password is never echoed and never becomes a command-line argument, so it
# stays out of shell history and out of `ps`.
hashpw:
	@read -rs -p "Admin password: " P; echo; printf %s "$$P" | go run ./cmd/hashpw

## sqlc: regenerate internal/db/gen from the queries and migrations
sqlc:
	$(SQLC) generate

## sqlc-check: fail if the checked-in generated code is stale
# Part of the gate before a commit. `sqlc diff` compares what would be generated
# against what is on disk, so a query edited without regenerating is caught
# before it is committed rather than by a reviewer noticing the SQL and the Go
# disagree.
sqlc-check:
	$(SQLC) diff

## sqlc-install: install the pinned sqlc
sqlc-install:
	go install github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION)

build:
	go build ./...

## test: run every test, including the database-backed ones
# -count=1: a cached pass after an environment change is not a pass.
test:
	TEST_DATABASE_URL="$(TEST_DATABASE_URL)" go test -count=1 ./...

vet:
	go vet ./...

fmt:
	go fmt ./...

tidy:
	go mod tidy

psql:
	$(COMPOSE) exec postgres psql -U goevent -d goevent

## image: build the production image locally, tagged with TAG
# Builds exactly what `publish` would push, without pushing it — for checking
# a Dockerfile change or running the real image locally.
image:
	docker build --platform $(PLATFORM) --label org.opencontainers.image.source=https://github.com/17xande-dev/goevent -t $(IMAGE):$(TAG) .

# check-clean: fail if the working tree has uncommitted changes. A published
# image has to be reproducible from source, and an image built from a dirty
# tree names a tag whose contents don't match what git says is at that tag.
check-clean:
	@if [ -n "$(ALLOW_DIRTY)" ]; then exit 0; fi; \
	if [ -n "$$(git status --porcelain)" ]; then \
	  echo "refusing to publish: uncommitted changes (commit them, or ALLOW_DIRTY=1)" >&2; \
	  git status --short >&2; \
	  exit 1; \
	fi

# check-tagged: fail unless TAG looks like a version (vX.Y.Z), so a forgotten
# `git tag` doesn't quietly publish a commit-sha image as if it were a release.
# Override with ALLOW_UNTAGGED=1 for a deliberate throwaway build.
check-tagged:
	@if [ -n "$(ALLOW_UNTAGGED)" ]; then exit 0; fi; \
	case "$(TAG)" in \
	  v[0-9]*) ;; \
	  *) echo "refusing to publish: HEAD is not tagged with a version — git tag vX.Y.Z && git push --tags, or pass TAG=vX.Y.Z (or ALLOW_UNTAGGED=1) explicitly" >&2; exit 1 ;; \
	esac

## publish: build the image and push TAG and latest to GHCR (requires docker login ghcr.io)
# Manual, on purpose — there is no workflow that runs this on a push or a tag.
# The release step is: tag, then `make publish` from a clean checkout of that
# tag; on the server, GOEVENT_VERSION in the deployment's .env points at the
# new tag and `docker compose pull && docker compose up -d` picks it up.
#
# GHCR defaults a newly pushed package to private. After the first publish,
# set it public in the package's GitHub settings (and "connect repository")
# or every deploy needs a PAT with read:packages.
publish: check-clean check-tagged image
	docker push $(IMAGE):$(TAG)
	docker tag $(IMAGE):$(TAG) $(IMAGE):latest
	docker push $(IMAGE):latest
	@echo
	@echo "pushed $(IMAGE):$(TAG) and $(IMAGE):latest"
