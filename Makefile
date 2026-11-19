# Divvy build tasks.
#
# The desktop UI (web/) is built with Node and its output (web/dist) is
# committed and embedded into the Go binary. A Go-only checkout builds and
# tests without Node, serving the committed dist; run `make web-build` when
# you change anything under web/src and commit the regenerated web/dist.

WEB_DIR := web

.PHONY: help web-install web-build go-build test test-race vet bench clean app

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

bench: ## Run the weak-model benchmark against a local Ollama (see docs/weak-model-bench.md)
	go run ./cmd/bench

app: ## Build the macOS desktop bundle (build/divvy.app, double-clickable)
	go run ./cmd/genicon -out cmd/divvy/icon_512.png
	mkdir -p build/divvy.app/Contents/MacOS build/divvy.app/Contents/Resources
	go build -tags production -o build/divvy.app/Contents/MacOS/divvy ./cmd/divvy
	sips -z 16 16     cmd/divvy/icon_512.png --out /tmp/icon_16.png  >/dev/null
	sips -z 32 32     cmd/divvy/icon_512.png --out /tmp/icon_32.png  >/dev/null
	sips -z 64 64     cmd/divvy/icon_512.png --out /tmp/icon_64.png  >/dev/null
	sips -z 128 128   cmd/divvy/icon_512.png --out /tmp/icon_128.png >/dev/null
	sips -z 256 256   cmd/divvy/icon_512.png --out /tmp/icon_256.png >/dev/null
	sips -z 512 512   cmd/divvy/icon_512.png --out /tmp/icon_512s.png >/dev/null
	cp cmd/divvy/icon_512.png /tmp/icon_512.png
	mkdir -p /tmp/divvy.iconset
	cp /tmp/icon_16.png  /tmp/divvy.iconset/icon_16x16.png
	cp /tmp/icon_32.png  /tmp/divvy.iconset/icon_16x16@2x.png
	cp /tmp/icon_32.png  /tmp/divvy.iconset/icon_32x32.png
	cp /tmp/icon_64.png  /tmp/divvy.iconset/icon_32x32@2x.png
	cp /tmp/icon_128.png /tmp/divvy.iconset/icon_128x128.png
	cp /tmp/icon_256.png /tmp/divvy.iconset/icon_128x128@2x.png
	cp /tmp/icon_512s.png /tmp/divvy.iconset/icon_256x256.png
	cp /tmp/icon_512.png /tmp/divvy.iconset/icon_512x512.png
	cp /tmp/icon_512.png /tmp/divvy.iconset/icon_512x512@2x.png
	iconutil -c icns /tmp/divvy.iconset -o build/divvy.app/Contents/Resources/divvy.icns
	sed -e 's/__VERSION__/0.1.0/' cmd/divvy/Info.plist.in > build/divvy.app/Contents/Info.plist
	@echo "built build/divvy.app — double-click or: open build/divvy.app"

clean: ## Remove web build artifacts and dependencies
	rm -rf $(WEB_DIR)/dist $(WEB_DIR)/node_modules $(WEB_DIR)/.vite build/divvy.app
