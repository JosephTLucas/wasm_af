.PHONY: demo demo-api demo-real reply-all-demo taint-demo build pull-model test-policy clean help

ROOT      := $(shell cd ../.. && pwd)
MODEL     ?= qwen3:4b
LLM_MODE  ?= mock

##@ Demo

demo: build ## Build + run with current LLM_MODE (default: mock)
	@LLM_MODE=$(LLM_MODE) $(ROOT)/examples/wasmclaw/run.sh

demo-api: build ## Build + run with NVIDIA NIM API (needs NV_API_KEY in .env or env)
	@LLM_MODE=api $(ROOT)/examples/wasmclaw/run.sh

demo-real: pull-model build ## Build + run with local Ollama
	@LLM_MODE=real MODEL=$(MODEL) $(ROOT)/examples/wasmclaw/run.sh

reply-all-demo: build ## Reply-all parallel DAG demo (jailbreak + approval in one task)
	@LLM_MODE=$(LLM_MODE) $(ROOT)/examples/wasmclaw/reply-all-demo.sh

taint-demo: build ## Taint tracking demo (needs LLM_MODE=api + BRAVE_API_KEY in .env)
	@LLM_MODE=api $(ROOT)/examples/wasmclaw/taint-demo.sh

##@ Setup

pull-model: ## Ensure the Ollama model is pulled (does nothing if already present)
	@command -v ollama >/dev/null 2>&1 || \
		(echo "Error: ollama not installed. Get it at https://ollama.com" && exit 1)
	@ollama show "$(MODEL)" >/dev/null 2>&1 || \
		(echo "Pulling $(MODEL) (this may take a few minutes)..." && ollama pull "$(MODEL)")

build: ## Build WASM agent components + Rust orchestrator
	@mkdir -p $(ROOT)/bin
	@echo "Building WASM agent components (wasm32-wasip2)..."
	@cd $(ROOT)/components && cargo build --release 2>&1
	@echo "Building Rust orchestrator..."
	@cd $(ROOT) && cargo build --release -p wasm-af-orchestrator 2>&1
	@cp $(ROOT)/target/release/orchestrator $(ROOT)/bin/orchestrator

##@ Test

test-policy: ## Run OPA unit tests for policy.rego and submit.rego (no services needed)
	@command -v opa >/dev/null 2>&1 || \
		(echo "Error: opa not installed. See https://www.openpolicyagent.org/docs/latest/#running-opa" && exit 1)
	@opa test $(ROOT)/examples/wasmclaw -v

##@ Util

clean: ## Kill any stale processes left from a previous interrupted run
	@lsof -ti:8080 | xargs kill 2>/dev/null || true

help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
		/^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)
