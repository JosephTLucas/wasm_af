# Fan-Out Summarizer

Fetches N URLs **in parallel** — each in its own WASM sandbox with a
per-instance network allow-list — then merges the results and produces
an LLM-generated summary.

## Quick start

```bash
./examples/fan-out-summarizer/run.sh
```

That's it. The script builds everything, starts NATS, runs the orchestrator,
submits the task, and prints formatted results. Prerequisites: Rust, Go, jq,
and either `nats-server` or `wash` (for its bundled NATS).

Custom URLs:

```bash
URLS="https://example.com,https://httpbin.org" ./examples/fan-out-summarizer/run.sh
```

## What it demonstrates

**Dynamic instantiation** — The orchestrator creates N url-fetch plugin
instances from a single .wasm binary, one per URL. The count is determined at
task submission time.

**Per-instance capability scoping** — Each url-fetch instance gets its own
Extism manifest with `allowed_hosts` set to only the domain of its assigned
URL. The Extism runtime rejects HTTP requests to unlisted hosts before they
leave the sandbox. This is enforced by the runtime, not application code.

**Parallel execution** — All fetch steps share a `group` tag. The orchestrator
dispatches them concurrently via goroutines, each in an independent WASM
sandbox.

**Ephemeral lifecycle** — Each plugin is created, invoked, and destroyed within
a single Go function scope. There is no persistent agent process between steps.
Memory (including any secrets) is reclaimed when the plugin closes.

**Policy gating** — The policy engine (itself a WASM plugin) evaluates
deny-by-default rules before each step. url-fetch is permitted HTTP; the
summarizer is permitted LLM. All other requests are denied.

**Capability isolation** — The summarizer receives an `llm_complete` host
function. The url-fetch plugin does not — the function literally doesn't exist
in its WASM instance. Even if injected code tries to call it, the import is
missing.

**Orchestrator-mediated merge** — Fetch results flow through NATS KV, not
direct plugin communication. The orchestrator merges N outputs into a single
context entry for the summarizer.

## How it works

```
POST /tasks { type: "fan-out-summarizer", urls: "A,B,C" }
  |
  +-- buildPlan() --> 3 url-fetch steps (group:"fetch") + 1 summarizer step
  |
  +-- runParallelSteps()
  |     +-- goroutine 0: policy --> create plugin(hosts:A) --> execute --> destroy
  |     +-- goroutine 1: policy --> create plugin(hosts:B) --> execute --> destroy
  |     +-- goroutine 2: policy --> create plugin(hosts:C) --> execute --> destroy
  |
  +-- mergeSearchOutputs() --> single web_search_results context entry
  |
  +-- runStep(summarizer): policy --> create plugin(+llm_complete host fn) --> execute --> destroy
```

## Files

- `policies.json` — deny-by-default policy rules (url-fetch -> HTTP, summarizer -> LLM)
- `run.sh` — builds, starts, runs, and displays the demo
- `README.md` — this file
