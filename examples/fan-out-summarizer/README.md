# Fan-Out Summarizer

Fetches N URLs in parallel — each in its own WASM sandbox with per-instance network scoping — then merges results and produces an LLM-generated summary.

```bash
./examples/fan-out-summarizer/run.sh
URLS="https://example.com,https://httpbin.org" ./examples/fan-out-summarizer/run.sh
```

Prerequisites: Rust (wasm32-wasip2 target), jq, `nats-server` (or `wash`). Optional: `nats` CLI for the live allowlist demo.

---

## Plan shape

```
POST /tasks { type: "fan-out-summarizer", urls: "A,B,C" }
  │
  ├── buildPlan():
  │     url-fetch(A)  ──┐
  │     url-fetch(B)  ──┼── summarizer(depends_on: [A, B, C])
  │     url-fetch(C)  ──┘
  │
  ├── DAG ready-set → all fetches run in parallel (no dependencies):
  │     ├── goroutine: policy → NewPlugin(hosts:[A]) → Call → Close
  │     ├── goroutine: policy → NewPlugin(hosts:[B]) → Call → Close
  │     └── goroutine: policy → NewPlugin(hosts:[C]) → Call → Close
  │
  └── DAG ready-set → summarizer (all deps met):
        policy → NewPlugin(+llm_complete) → Call → Close
```

Each url-fetch plugin's `allowed_hosts` is set to exactly one domain. The summarizer gets `llm_complete` but no HTTP capability. These are manifest-level constraints — not application checks.

Two network enforcement layers address different threats: the server-side domain allowlist (NATS KV) gates task submission; per-instance `allowed_hosts` (wasmtime `WasiHttpView`) gates plugin HTTP at the runtime syscall layer. Live updates: `nats kv put wasm-af-config allowed-fetch-domains "domain1,domain2"`.

## Files

- `agents.json` — agent registry (wasm names, capabilities, host functions, payload fields)
- `policy.rego` / `submit.rego` — step and submission policies
- `data.json` — domain allowlist, allowed task types
- `*_test.rego` — OPA tests (`opa test .`)
- `run.sh` — builds, starts, runs, and displays the demo
