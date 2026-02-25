.PHONY: build build-orchestrator build-plugins test demo clean help

BINDIR := bin

##@ Build

build: build-orchestrator build-plugins ## Build everything

build-orchestrator: ## Build Go orchestrator binary
	@mkdir -p $(BINDIR)
	go build -o $(BINDIR)/orchestrator ./provider/orchestrator/

build-plugins: ## Build Rust WASM plugins (Extism)
	@cd components && cargo build --release
	@echo "WASM plugins built in components/target/wasm32-unknown-unknown/release/"

##@ Test

test: ## Run Go unit tests
	go test ./pkg/...

test-plugins: ## Run Rust plugin unit tests (native target override)
	@cd components && cargo test --target "$$(rustc -vV | grep host | awk '{print $$2}')"

##@ Run

demo: build ## Run the fan-out-summarizer example end-to-end
	./examples/fan-out-summarizer/run.sh

##@ Util

clean: ## Remove build artifacts
	rm -rf $(BINDIR)
	cd components && cargo clean

help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
		/^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)
