# A Self-Documenting Makefile: http://marmelab.com/blog/2016/02/29/auto-documented-makefile.html

export PATH := $(abspath bin/):${PATH}

##@ General

# Targets commented with ## will be visible in "make help" info.
# Comments marked with ##@ will be used as categories for a group of targets.

.PHONY: help
default: help
help: ## Display this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: up
up: ## Start development environment
	docker compose up -d

.PHONY: down
down: ## Destroy development environment
	docker compose down -v

##@ Checks

.PHONY: check
check: lint test ## Run lint checks and tests

.PHONY: lint
lint: lint-go lint-yaml
lint: ## Run linters

.PHONY: lint-go
lint-go:
	$(GOLANGCI_LINT_BIN) run $(if ${CI},--out-format colored-line-number,)

.PHONY: lint-yaml
lint-yaml:
	$(YAMLLINT_BIN) $(if ${CI},-f github,) --no-warnings .

.PHONY: test
test: test-sdk test-injector-vault test-injector-bao ## Run tests

targets := $(shell go list ./... | grep -v '/injector')

.PHONY: test-sdk
test-sdk: ## Run tests for SDK
	go test -race -v $(targets)

.PHONY: test-injector-vault
test-injector-vault: up ## Run tests for vault-injector
	go test -race -v ./injector/vault

.PHONY: test-injector-bao
test-injector-bao: up ## Run tests for bao-injector
	go test -race -v ./injector/bao

.PHONY: fmt
fmt: ## Format code
	$(GOLANGCI_LINT_BIN) run --fix

.PHONY: license-check
license-check: ## Run license check
	$(LICENSEI_BIN) check
	$(LICENSEI_BIN) header

##@ Dependencies

# Dependency versions
GOLANGCI_LINT_VERSION = 1.60.3
LICENSEI_VERSION = 0.9.0

# Dependency binaries
GOLANGCI_LINT_BIN := golangci-lint
LICENSEI_BIN := licensei

# TODO: add support for yamllint dependency
YAMLLINT_BIN := yamllint

deps: bin/golangci-lint bin/licensei
deps: ## Install dependencies

# If we have "bin" dir, use those binaries instead
ifneq ($(wildcard ./bin/.),)
	GOLANGCI_LINT_BIN := bin/$(GOLANGCI_LINT_BIN)
	LICENSEI_BIN := bin/$(LICENSEI_BIN)
endif

bin/golangci-lint:
	@mkdir -p bin
	curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | bash -s -- v${GOLANGCI_LINT_VERSION}

bin/licensei:
	@mkdir -p bin
	curl -sfL https://raw.githubusercontent.com/goph/licensei/master/install.sh | bash -s -- v${LICENSEI_VERSION}
