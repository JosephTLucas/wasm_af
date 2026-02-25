# Fan-Out Summarizer

Fetches N URLs in parallel — each in its own WASM sandbox — then merges results and produces an LLM-generated summary. One command, end-to-end.

```bash
./examples/fan-out-summarizer/run.sh
```

Custom URLs:

```bash
URLS="https://example.com,https://httpbin.org" ./examples/fan-out-summarizer/run.sh
```

Prerequisites: Rust, Go 1.24+, jq, and either `nats-server` or `wash` (for its bundled NATS). The `nats` CLI ([natscli](https://github.com/nats-io/natscli)) is optional but required for the live allow-list demo section.

---

## What it demonstrates

**Per-instance capability scoping.** Each url-fetch plugin gets its own Extism manifest with `allowed_hosts` set to exactly one domain. The runtime rejects HTTP requests to any other host before they leave the sandbox. Instance A physically cannot reach Instance B's domain.

**Policy gating.** The policy engine — itself a WASM plugin — evaluates every step before a plugin is instantiated. Deny-by-default: if there's no matching rule, the step doesn't run. The policy engine has no access to task data or credentials.

**Physical capability isolation.** The summarizer receives an `llm_complete` host function. The url-fetch plugins do not — the function doesn't exist in their WASM instances. The summarizer has no HTTP capability. These aren't advisory rules; they're what's in each plugin's manifest.

**Defense-in-depth network enforcement.** Two distinct layers, addressing different threat actors:

- **Server-side allowlist** (NATS KV → `buildPlan`): checked against the domains in the task request before any plugin is instantiated. Guards against the *task submitter* requesting a hostile domain. If the domain isn't listed, `runStep` is never called — zero WASM bytecode executes.
- **Per-instance `allowed_hosts`** (Extism manifest): enforced by the wazero runtime when a plugin makes an HTTP call. Guards against *plugin code* (e.g. a prompt-injected agent) trying to reach a domain outside its assignment. The plugin runs, but the network call fails at the syscall layer.

Neither layer alone is sufficient: the allowlist doesn't constrain what plugin code does internally; `allowed_hosts` doesn't prevent a caller from submitting hostile URLs before the plugin starts. Live allowlist updates (no restart): `nats kv put wasm-af-config allowed-fetch-domains "domain1,domain2"`.

**Ephemeral lifecycle.** Each plugin is `NewPlugin → Call("execute") → Close` within a single goroutine scope. Between steps, there is no running process to attack, no memory to dump.

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

## Files

- `policies.json` — deny-by-default policy rules
- `run.sh` — builds, starts, runs, and displays the demo
