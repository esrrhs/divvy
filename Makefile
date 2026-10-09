# Divvy build tasks.
#
# The desktop UI (web/) is built with Node and its output (web/dist) is
# committed and embedded into the Go binary. A Go-only checkout builds and
# tests without Node, serving the committed dist; run `make web-build` when
# you change anything under web/src and commit the regenerated web/dist.

WEB_DIR := web

.PHONY: help web-install web-build go-build test test-race vet clean

help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  %-12s %s\n", $$1, $$2}'

# Reinstall only when the lockfile changes.
$(WEB_DIR)/node_modules: $(WEB_DIR)/package-lock.json
	cd $(WEB_DIR) && npm ci

web-install: ## Install web dependencies (npm ci)
web-install: $(WEB_DIR)/node_modules

web-build: ## Build the UI into web/dist (run after editing web/src)
web-build: $(WEB_DIR)/node_modules
	cd $(WEB_DIR) && npm run build

go-build: ## Build all Go packages (embeds committed web/dist)
	go build ./...

vet: ## Run go vet
	go vet ./...

test: ## Run Go tests
	go test ./... -count=1

test-race: ## Run Go tests with the race detector
	go test -race ./... -count=1

clean: ## Remove web build artifacts and dependencies
	rm -rf $(WEB_DIR)/dist $(WEB_DIR)/node_modules $(WEB_DIR)/.vite
