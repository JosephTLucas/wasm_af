.PHONY: demo build test-policy clean help

ROOT    := $(shell cd ../.. && pwd)

##@ Demo

demo: ## Build everything, run OPA policy tests, and start the wasmclaw demo
	@$(ROOT)/examples/wasmclaw/run.sh

##@ Setup

build: ## Build wasmclaw WASM agents, orchestrator, and webhook-gateway
	@mkdir -p $(ROOT)/bin
	@echo "Building Rust WASM agents (unknown-unknown)..."
	@cd $(ROOT)/components && cargo build --release -p router -p shell -p memory -p responder 2>&1
	@echo "Building file-ops (wasm32-wasip1 → WASI filesystem)..."
	@cd $(ROOT)/components && cargo build --release -p file-ops --target wasm32-wasip1 2>&1
	@cp $(ROOT)/components/target/wasm32-wasip1/release/file_ops.wasm \
		$(ROOT)/components/target/wasm32-unknown-unknown/release/file_ops.wasm
	@echo "Building Go orchestrator..."
	@go build -o $(ROOT)/bin/orchestrator $(ROOT)/provider/orchestrator/ 2>&1
	@echo "Building Go webhook-gateway..."
	@go build -o $(ROOT)/bin/webhook-gateway $(ROOT)/cmd/webhook-gateway/ 2>&1

##@ Test

test-policy: ## Run OPA unit tests for policy.rego and submit.rego (no services needed)
	@command -v opa >/dev/null 2>&1 || \
		(echo "Error: opa not installed. See https://www.openpolicyagent.org/docs/latest/#running-opa" && exit 1)
	@opa test $(ROOT)/examples/wasmclaw -v

##@ Util

clean: ## Kill any stale processes left from a previous interrupted run
	@lsof -ti:8080 | xargs kill 2>/dev/null || true
	@lsof -ti:8081 | xargs kill 2>/dev/null || true

help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
		/^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)
