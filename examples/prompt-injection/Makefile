.PHONY: demo build pull-model clean help

ROOT    := $(shell cd ../.. && pwd)
MODEL   ?= gemma3:270m

##@ Demo

demo: pull-model build ## Pull model, build everything, run the prompt injection demo
	@MODEL=$(MODEL) $(ROOT)/examples/prompt-injection/run.sh

##@ Setup

pull-model: ## Ensure the Ollama model is pulled (does nothing if already present)
	@command -v ollama >/dev/null 2>&1 || \
		(echo "Error: ollama not installed. Get it at https://ollama.com" && exit 1)
	@ollama show "$(MODEL)" >/dev/null 2>&1 || \
		(echo "Pulling $(MODEL) (this may take a few minutes)..." && ollama pull "$(MODEL)")

build: ## Build the Rust orchestrator and WASM agent components
	@mkdir -p $(ROOT)/bin
	@echo "Building WASM agent components..."
	@cd $(ROOT)/components && cargo build --release 2>&1
	@echo "Building Rust orchestrator..."
	@cd $(ROOT) && cargo build --release -p wasm-af-orchestrator 2>&1
	@cp $(ROOT)/target/release/orchestrator $(ROOT)/bin/orchestrator

##@ Util

clean: ## Kill any stale processes left from a previous interrupted run
	@lsof -ti:8765 | xargs kill 2>/dev/null || true
	@lsof -ti:8080 | xargs kill 2>/dev/null || true

help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
		/^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)
