.PHONY: build build-providers build-components test test-integration deploy-local clean

GOFLAGS := CGO_ENABLED=0
GOOS    := $(shell go env GOOS)
GOARCH  := $(shell go env GOARCH)
BINDIR  := bin

ORCHESTRATOR_BIN := $(BINDIR)/orchestrator
LLM_BIN          := $(BINDIR)/llm-inference

REGISTRY := localhost:5000

##@ Build

build: build-providers build-components ## Build everything

build-providers: ## Build Go provider binaries
	@mkdir -p $(BINDIR)
	$(GOFLAGS) go build -o $(ORCHESTRATOR_BIN) ./provider/orchestrator/
	$(GOFLAGS) go build -o $(LLM_BIN)          ./provider/llm-inference/

build-components: ## Build Rust WASM components
	@cd components && cargo build --target wasm32-wasip2 --release
	@echo "WASM components built in components/target/wasm32-wasip2/release/"

##@ Test

test: ## Run Go unit tests
	go test ./pkg/... ./provider/...

test-integration: ## Run integration tests (requires running wash up)
	go test -tags integration ./pkg/... ./provider/...

##@ Deploy

deploy-local: build ## Deploy to local wasmCloud (requires wash up + local OCI registry)
	wash app deploy deploy/wadm.yaml

push-components: build-components ## Push WASM components to local OCI registry
	wash push $(REGISTRY)/policy-engine:latest \
		components/target/wasm32-wasip2/release/policy_engine.wasm
	wash push $(REGISTRY)/agent-web-search:latest \
		components/target/wasm32-wasip2/release/web_search.wasm
	wash push $(REGISTRY)/agent-summarizer:latest \
		components/target/wasm32-wasip2/release/summarizer.wasm

##@ Dev

wash-up: ## Start local wasmCloud + NATS
	wash up --detached

wash-down: ## Stop local wasmCloud
	wash down

registry: ## Start local OCI registry
	docker run -d -p 5000:5000 --name wasm-af-registry registry:2 || \
		docker start wasm-af-registry

##@ Util

clean: ## Remove build artifacts
	rm -rf $(BINDIR)
	cd components && cargo clean

help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
		/^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)
