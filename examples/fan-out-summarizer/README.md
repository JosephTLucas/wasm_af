# Fan-Out Summarizer

Fetches N URLs in parallel — each in its own WASM sandbox with per-instance network scoping — then merges results and produces an LLM-generated summary.

```bash
./examples/fan-out-summarizer/run.sh
URLS="https://example.com,https://httpbin.org" ./examples/fan-out-summarizer/run.sh
```

Prerequisites: Rust, Go 1.25+, jq, `nats-server` (or `wash`). Optional: `nats` CLI for the live allowlist demo.

---

## Plan shape

```
POST /tasks { type: "fan-out-summarizer", urls: "A,B,C" }
  │
  ├── buildPlan()  →  [url-fetch(A), url-fetch(B), url-fetch(C)] + [summarizer]
  │
  ├── runParallelSteps()  (group: "fetch")
  │     ├── goroutine: policy → NewPlugin(hosts:[A]) → Call → Close
  │     ├── goroutine: policy → NewPlugin(hosts:[B]) → Call → Close
  │     └── goroutine: policy → NewPlugin(hosts:[C]) → Call → Close
  │
  ├── merge outputs → single "web_search_results" context entry
  │
  └── runStep(summarizer): policy → NewPlugin(+llm_complete) → Call → Close
```

Each url-fetch plugin's `allowed_hosts` is set to exactly one domain. The summarizer gets `llm_complete` but no HTTP capability. These are manifest-level constraints — not application checks.

Two network enforcement layers address different threats: the server-side domain allowlist (NATS KV) gates task submission; per-instance `allowed_hosts` (Extism manifest) gates plugin HTTP at the wazero syscall layer. Live updates: `nats kv put wasm-af-config allowed-fetch-domains "domain1,domain2"`.

## Files

- `policy.rego` / `submit.rego` — step and submission policies
- `data.json` — domain allowlist, allowed task types
- `*_test.rego` — OPA tests (`opa test .`)
- `run.sh` — builds, starts, runs, and displays the demo
