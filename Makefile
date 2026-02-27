.PHONY: build build-orchestrator build-plugins build-gateway wasmclaw test demo wasmclaw-demo wasmclaw-demo-api wasmclaw-demo-real clean help

BINDIR := bin

##@ Build

build: build-orchestrator build-plugins build-gateway ## Build everything

build-orchestrator: ## Build Go orchestrator binary
	@mkdir -p $(BINDIR)
	go build -o $(BINDIR)/orchestrator ./provider/orchestrator/

build-gateway: ## Build webhook-gateway binary
	@mkdir -p $(BINDIR)
	go build -o $(BINDIR)/webhook-gateway ./cmd/webhook-gateway/

build-plugins: ## Build Rust WASM plugins (Extism)
	@cd components && cargo build --release
	@echo "WASM plugins built in components/target/wasm32-unknown-unknown/release/"

wasmclaw: ## Build wasmclaw agents (router, shell, file-ops, memory, responder, sandbox-exec, email-*) + gateway + runtimes
	@mkdir -p $(BINDIR)
	@# router, shell, memory, responder, sandbox-exec, email-send, email-read: no WASI stdlib → unknown-unknown
	@cd components && cargo build --release -p router -p shell -p memory -p responder -p sandbox-exec -p email-send -p email-read
	@# file-ops: uses std::fs via WASI filesystem API → wasip1
	@cd components && cargo build --release -p file-ops --target wasm32-wasip1
	@cp components/target/wasm32-wasip1/release/file_ops.wasm \
		components/target/wasm32-unknown-unknown/release/file_ops.wasm
	go build -o $(BINDIR)/orchestrator ./provider/orchestrator/
	go build -o $(BINDIR)/webhook-gateway ./cmd/webhook-gateway/
	@# Download Python WASM runtime for sandbox-exec (if not already present)
	@if [ ! -f runtimes/python.wasm ]; then bash runtimes/build.sh; fi
	@echo "wasmclaw build complete."
	@echo "  WASM (unknown-unknown): router, shell, memory, responder, sandbox-exec, email-send, email-read"
	@echo "  WASM (wasip1 → copied): file_ops"
	@echo "  Sandbox runtime: runtimes/python.wasm"
	@echo "  Gateway: $(BINDIR)/webhook-gateway"

##@ Test

test: ## Run all Go unit tests (orchestrator + pkg)
	go test ./provider/orchestrator/ ./pkg/...

test-plugins: ## Run Rust plugin unit tests (native target override)
	@cd components && cargo test --target "$$(rustc -vV | grep host | awk '{print $$2}')"

test-policy: ## Run OPA policy tests for all examples
	@command -v opa >/dev/null 2>&1 || (echo "Error: opa not installed" && exit 1)
	@for d in examples/*/; do \
		if ls "$$d"*_test.rego >/dev/null 2>&1; then \
			echo "=== $$d ===" && opa test "$$d" -v; \
		fi; \
	done

##@ Run

demo: build ## Run the fan-out-summarizer example end-to-end
	./examples/fan-out-summarizer/run.sh

wasmclaw-demo: wasmclaw ## Build and run wasmclaw (mock LLM)
	./examples/wasmclaw/run.sh

wasmclaw-demo-api: wasmclaw ## Build and run wasmclaw with NVIDIA NIM API (needs NV_API_KEY)
	LLM_MODE=api ./examples/wasmclaw/run.sh

wasmclaw-demo-real: wasmclaw ## Build and run wasmclaw with local Ollama
	LLM_MODE=real ./examples/wasmclaw/run.sh

##@ Util

clean: ## Remove build artifacts
	rm -rf $(BINDIR)
	cd components && cargo clean

help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
		/^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)
