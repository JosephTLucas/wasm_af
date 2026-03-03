.PHONY: build build-orchestrator build-plugins wasmclaw lint lint-check fmt test demo wasmclaw-demo wasmclaw-demo-api wasmclaw-demo-real clean install-hooks help

BINDIR := bin

##@ Build

build: build-orchestrator build-plugins ## Build everything

build-orchestrator: ## Build Rust orchestrator binary
	@mkdir -p $(BINDIR)
	cargo build --release -p wasm-af-orchestrator
	@cp target/release/orchestrator $(BINDIR)/orchestrator

build-plugins: ## Build WASM agent components (Component Model, wasm32-wasip2)
	@cd components && cargo build --release
	@echo "WASM components built in components/target/wasm32-wasip2/release/"

wasmclaw: build ## Build wasmclaw agents + orchestrator + runtimes
	@if [ ! -f runtimes/python.wasm ]; then bash runtimes/build.sh; fi
	@echo "wasmclaw build complete."
	@echo "  WASM components (wasip2): router, shell, file-ops, memory, responder, sandbox-exec, email-send, email-read, web-search"
	@echo "  Sandbox runtime: runtimes/python.wasm"

##@ Lint

lint: ## Run clippy on all Rust crates
	cargo clippy --workspace -- -D warnings
	@cd components && cargo clippy --workspace -- -D warnings

lint-check: ## Run all CI lint checks locally (fmt + clippy + opa fmt)
	cargo fmt --all --check
	@cd components && cargo fmt --check
	cargo clippy --workspace -- -D warnings
	@HOST_TARGET=$$(rustc -vV | grep host | awk '{print $$2}'); \
	 cd components && cargo clippy --target "$$HOST_TARGET" -- -D warnings
	@if command -v opa >/dev/null 2>&1; then \
	  BAD=""; \
	  for f in $$(find . -name '*.rego' -not -path '*/target/*'); do \
	    if ! opa fmt --fail "$$f" >/dev/null 2>&1; then BAD="$$BAD $$f"; fi; \
	  done; \
	  if [ -n "$$BAD" ]; then echo "opa fmt needed on:$$BAD"; exit 1; fi; \
	  echo "opa fmt: ok"; \
	else echo "opa not installed — skipping"; fi

fmt: ## Format all Rust code
	cargo fmt --all
	@cd components && cargo fmt --all

##@ Test

test: ## Run all Rust tests
	cargo test --workspace
	@cd components && cargo test --workspace --target "$$(rustc -vV | grep host | awk '{print $$2}')"

test-policy: ## Run OPA unit tests (wasmclaw)
	@command -v opa >/dev/null 2>&1 || (echo "opa not installed" && exit 1)
	@opa test examples/wasmclaw -v

##@ Run

demo: wasmclaw ## Run wasmclaw demo (mock LLM)
	@cd examples/wasmclaw && make demo

wasmclaw-demo: wasmclaw ## Run wasmclaw demo (mock LLM)
	@cd examples/wasmclaw && make demo

wasmclaw-demo-api: wasmclaw ## Run wasmclaw with NVIDIA NIM API
	@cd examples/wasmclaw && LLM_MODE=api make demo

wasmclaw-demo-real: wasmclaw ## Run wasmclaw with local Ollama
	@cd examples/wasmclaw && LLM_MODE=real make demo

reply-all-demo: wasmclaw ## Run reply-all parallel DAG demo
	@cd examples/wasmclaw && make reply-all-demo

##@ Util

install-hooks: ## Install git pre-commit hook (mirrors CI lint stage)
	@cp hooks/pre-commit .git/hooks/pre-commit
	@chmod +x .git/hooks/pre-commit
	@echo "pre-commit hook installed"

clean: ## Kill stale processes + clean build artifacts
	@lsof -ti:8080 | xargs kill 2>/dev/null || true
	cargo clean
	@cd components && cargo clean

help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
		/^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)
