# PII Pipeline — Bring Your Own Agent Demo

A user writes a PII redactor in Python, compiles it to a WASM component, uploads it to a running orchestrator, and executes it in a three-step pipeline alongside platform agents.

```
url-fetch (platform, HTTP) → pii-redactor (BYOA, Python) → responder (platform, LLM)
```

## What it demonstrates

- **Full BYOA lifecycle**: write Python → `componentize-py` → `POST /agents` → execute
- **Trust-tier composition**: untrusted (BYOA) + trusted (platform) agents in one DAG
- **Approval gate**: OPA policy requires human approval before the BYOA agent runs
- **Structural capability isolation**: the Python agent has no host functions — no LLM, no HTTP, no shell. Those capabilities are absent from its wasmtime Linker, not just denied at runtime.
- **Index-based DAG construction**: external clients build custom step pipelines using `context.steps` without knowing the server-generated task ID

## Quick start

```bash
cd examples/pii-pipeline
make demo                      # mock LLM (deterministic, no external deps)
LLM_MODE=api make demo         # NVIDIA NIM API (needs NV_API_KEY)
```

Prerequisites: Rust (with `wasm32-wasip2` target), NATS, jq, Python 3, Docker (for building the Python agent on macOS 26+) or `componentize-py`.

## Pipeline

| Step | Agent | Capability | Host Functions | Role |
|------|-------|-----------|----------------|------|
| 0 | `url-fetch` | `http` | `[http]` | Fetches the sample document over HTTP |
| 1 | `pii-redactor` | `untrusted` | `[]` | Scans for SSN, email, phone, CC, IP — pure computation |
| 2 | `responder` | `llm` | `[llm_complete]` | Summarizes the PII findings |

## The Python agent

The agent is `agent/app.py` — about 90 lines of Python using only `json` and `re` from the standard library. It implements the `agent-untrusted` WIT world (no host function imports beyond `host-config`).

### Building

```bash
# Native (fast, if componentize-py works on your OS):
pip3 install componentize-py
cd agent && componentize-py -d ../../../wit/agent.wit -w agent-untrusted componentize app -o pii_redactor.wasm

# Docker (works on macOS 26+ where native crashes due to Mach port guards):
BUILD_METHOD=docker bash agent/build.sh
```

### Uploading

```bash
curl -X POST localhost:8080/agents \
  -F 'meta={"name":"pii-redactor","context_key":"pii_result"}' \
  -F 'wasm=@agent/pii_redactor.wasm'
```

## OPA policy

The demo policy (`policy.rego`) enforces:
- url-fetch: allowed, HTTP capability, localhost only
- responder: allowed, LLM capability
- pii-redactor (untrusted): allowed if in `approved_external_agents`, 256 pages (16 MiB) memory, 10s timeout, no host functions, **requires human approval**

## Files

```
examples/pii-pipeline/
├── Makefile                # demo, build, test-policy, clean
├── run.sh                  # end-to-end demo script
├── agents.json             # platform agent registry (url-fetch, responder)
├── policy.rego             # OPA policy (platform + BYOA rules)
├── submit.rego             # submission policy (allowed task types)
├── data.json               # config (allowed domains, approved agents)
├── sample-doc.html         # sample document with PII
└── agent/
    ├── app.py              # Python PII redactor (90 lines)
    └── build.sh            # componentize-py build script (native + Docker)
```
