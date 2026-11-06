# smokealarm: common tasks. Run "make help" for the list.

IMAGE      ?= ghcr.io/gagaschu/smokealarm
TAG        ?= dev
PLATFORMS  ?= linux/amd64,linux/arm64
GO_VERSION := $(shell awk '/^go /{print $$2}' go.mod)

# "make hash-password" uses "read -s", which is not in plain POSIX sh.
SHELL := bash

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this list
	@awk 'BEGIN{FS=":.*## "} /^[a-z-]+:.*## /{printf "  %-14s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: build
build: ## Build the binary into bin/smokealarm
	CGO_ENABLED=0 go build -trimpath -o bin/smokealarm ./cmd/smokealarm

.PHONY: test
test: ## Run go vet and all tests
	go vet ./...
	go test ./...

# On Linux the race detector needs cgo, that is a C compiler ("go: -race requires
# cgo"). The build itself does not (CGO_ENABLED=0), which is why "make test"
# works without one.
.PHONY: test-race
test-race: ## Run the tests with the race detector (needs a C compiler on Linux)
	go vet ./...
	go test -race ./...

.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run

# The images in docs/screenshots, used by the README and the docs. Made-up data, rendered by
# the real handlers. Needs Google Chrome or Chromium (CHROME=/path/to/chrome if
# it is not found) and pngquant to keep the files small.
.PHONY: screenshots
screenshots: ## Take the screenshots for the docs again (needs Chrome and pngquant)
	UPDATE_SCREENSHOTS=1 go test ./internal/web -run TestScreenshots -count=1 -v

# Local run against the cluster from KUBECONFIG. The password hash is required;
# create one with "make hash-password". The defaults suit local development:
# readable logs and a session cookie that also works over plain HTTP.
.PHONY: run
run: ## Run locally against KUBECONFIG (needs SMOKEALARM_ADMIN_PASSWORD_HASH)
	@test -n "$$SMOKEALARM_ADMIN_PASSWORD_HASH" || { \
	  echo 'SMOKEALARM_ADMIN_PASSWORD_HASH is not set. Create one with "make hash-password" and export it.'; exit 1; }
	SMOKEALARM_ADMIN_USER=$${SMOKEALARM_ADMIN_USER:-admin} \
	SMOKEALARM_LOG_FORMAT=$${SMOKEALARM_LOG_FORMAT:-text} \
	SMOKEALARM_COOKIE_SECURE=$${SMOKEALARM_COOKIE_SECURE:-false} \
	go run ./cmd/smokealarm

.PHONY: hash-password
hash-password: ## Ask for a password and print its bcrypt hash
	@read -s -p 'Password: ' pw; echo >&2; printf '%s' "$$pw" | go run ./cmd/smokealarm hash-password

# Multi-architecture image (amd64 and arm64). This needs a buildx builder that
# supports multiple platforms (create one once with "docker buildx create --use",
# or turn on the containerd image store in Docker Desktop). Without PUSH=1 it
# is only built; a multi-arch image cannot be loaded into the local Docker, use
# "make image-local" for that. Example for publishing:
#   make image IMAGE=ghcr.io/you/smokealarm TAG=0.1.0 PUSH=1
.PHONY: image
image: ## Build the multi-arch image (PUSH=1 to push it)
	docker buildx build --platform $(PLATFORMS) --build-arg GO_VERSION=$(GO_VERSION) \
	  -t $(IMAGE):$(TAG) $(if $(PUSH),--push,) .

.PHONY: image-local
image-local: ## Build the image for this machine and load it into Docker
	docker buildx build --load --build-arg GO_VERSION=$(GO_VERSION) -t $(IMAGE):$(TAG) .

# A made-up hash: lint and template do not check it, they only need a value.
CHART_HASH := --set-string auth.passwordHash='$$2a$$10$$example'
CHART      := deploy/helm/smokealarm

.PHONY: chart
chart: ## Lint and render the Helm chart with the defaults and with each file in deploy/helm/examples
	helm lint $(CHART) $(CHART_HASH)
	helm template smokealarm $(CHART) $(CHART_HASH) > /dev/null
	@for file in deploy/helm/examples/*.yaml; do \
	  echo "== $$file"; \
	  helm lint $(CHART) $(CHART_HASH) -f $$file || exit 1; \
	  helm template smokealarm $(CHART) $(CHART_HASH) -f $$file > /dev/null || exit 1; \
	done
