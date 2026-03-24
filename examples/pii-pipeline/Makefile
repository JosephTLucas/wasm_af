.PHONY: demo demo-api build build-agent test-policy clean help

ROOT      := $(shell cd ../.. && pwd)
LLM_MODE  ?= mock

##@ Demo

demo: build ## Build + run with current LLM_MODE (default: mock)
	@LLM_MODE=$(LLM_MODE) $(ROOT)/examples/pii-pipeline/run.sh

demo-api: build ## Build + run with remote API (needs LLM_API_KEY + LLM_BASE_URL in .env or env)
	@LLM_MODE=api $(ROOT)/examples/pii-pipeline/run.sh

##@ Setup

build: build-agent ## Build everything (platform agents + orchestrator + BYOA agent)
	@mkdir -p $(ROOT)/bin
	@echo "Building WASM agent components (wasm32-wasip2)..."
	@cd $(ROOT)/components && cargo build --release 2>&1
	@echo "Building Rust orchestrator..."
	@cd $(ROOT) && cargo build --release -p wasm-af-orchestrator 2>&1
	@cp $(ROOT)/target/release/orchestrator $(ROOT)/bin/orchestrator

build-agent: ## Build the Python PII Redactor BYOA agent
	@bash $(ROOT)/examples/pii-pipeline/agent/build.sh

##@ Test

test-policy: ## Run OPA unit tests for the PII pipeline policies
	@command -v opa >/dev/null 2>&1 || \
		(echo "Error: opa not installed. See https://www.openpolicyagent.org/docs/latest/#running-opa" && exit 1)
	@opa test $(ROOT)/examples/pii-pipeline -v

##@ Util

clean: ## Kill any stale processes from a previous run
	@lsof -ti:8080 | xargs kill 2>/dev/null || true
	@lsof -ti:8888 | xargs kill 2>/dev/null || true
	@lsof -ti:4222 | xargs kill 2>/dev/null || true

help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
		/^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)
