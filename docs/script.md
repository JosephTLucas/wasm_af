# WASM_AF Lightning Talk — 10 Minutes
## "No Ambient Authority: Sandboxed AI Agents with WebAssembly"
### Audience: NVIDIA NeMo Agent Toolkit developers

---

> **Timing guide:** Each section has a target. Sections marked ⚡ have code on-screen.
> Speak at ~130 words/minute. This script is ~1,350 words.

---

## [0:00–1:00] The Hook

You're all building agent frameworks. You've added tool call validation, output guardrails, rate limiting, content classifiers. You're doing the right things.

But here's what keeps me up at night: all of those guardrails are enforced in the same process as the agent. The same Python interpreter. The same memory space. The same network socket.

If a prompt injection bypasses your output filter, the agent still has access to every environment variable in that process. Every credential your orchestrator loaded at startup. Every other agent's context buffer sitting in the heap.

You've built very good walls *inside* a house with no exterior walls.

I want to show you a different execution model — one where the walls are a property of the runtime, not of the code running in it.

---

## [1:00–2:00] What WASM Is (For Docker People)

You know Docker. A container packages an application and isolates it from the host using Linux kernel namespaces. It works at the OS level.

WebAssembly is the same idea applied one layer down — at the instruction set level.

A WASM module is a binary that runs in a sandboxed virtual machine. It **cannot** touch the filesystem, the network, environment variables, or system clocks unless the host explicitly provides those capabilities through a typed interface. Not "it's not supposed to." Not "we've added middleware." **It cannot.** The instruction set doesn't have the opcodes.

The analogy to Docker: if Docker's container escape vulnerability means someone got root on your host, in WASM that attack surface doesn't exist. There's no kernel boundary to cross. There's no OS.

WASI — the WebAssembly System Interface — is the typed permission layer. It defines *how* a WASM component requests capabilities: not as ambient system calls, but as imported functions that the host provides at runtime. No import, no access.

---

## [2:00–3:30] The WASM_AF Model ⚡

WASM_AF runs AI agents as WASM components on wasmCloud — a CNCF runtime for WASM. Here's the core loop:

```
POST /tasks → build plan → for each step:
  1. policy.evaluate(source, target, capability)  → permit | deny
  2. ctl.StartComponent(host, ociRef, componentID)
  3. ctl.PutLink(component → capability-provider)
  4. wRPC execute(task-input)  → task-output
  5. store.Update(task, step, results)
  6. ctl.DeleteLink(...)
  7. ctl.StopComponent(...)
```
*`docs/architecture.md` — Data Flow*

Notice steps 6 and 7. The agent is **destroyed** after every step. Not suspended. Not cached. Destroyed. Its memory is gone. Its in-memory secrets are gone. Its network connections are gone.

An agent that doesn't exist can't be exploited between invocations.

Cold starts are sub-millisecond. This isn't Docker — there's no kernel to boot, no filesystem to layer. A 115 KB WASM binary loads and executes in under a millisecond. So we can afford to treat lifecycle as a security primitive, not just a performance knob.

---

## [3:30–5:00] Policy Before Every Link ⚡

Every capability grant goes through a policy engine. The interesting part: **the policy engine is itself a WASM component.** It's immutable, versioned via OCI, and has no ambient authority of its own.

```rust
// components/policy-engine/src/lib.rs:107–125

impl Guest for Component {
    fn evaluate(req: LinkRequest) -> Result<Permit, DenyReason> {
        let policy = load_policy()?;
        let cap_str = capability_str(req.capability);

        for rule in &policy.rules {
            if rule.matches(&req.source_component_id,
                            &req.target_component_id, cap_str) {
                let mode = comms_mode_from_str(&rule.comms_mode, ...)?;
                return Ok(Permit { comms_mode: mode });
            }
        }

        Err(DenyReason {
            code: DenyCode::NotAllowed,
            message: format!("no policy rule permits {context}; deny-by-default"),
        })
    }
}
```

Deny-by-default. First-match-wins. The policy rules are JSON delivered at evaluate-time through `wasi:config/runtime` — the component reads them fresh on every call. No stale cached decisions.

The policy rule set for our demo research pipeline:

```json
// deploy/policies/default.json
{ "rules": [
  { "source": "wasm-af:web-search", "target": "*",
    "capability": "http",   "comms_mode": "mediated" },
  { "source": "wasm-af:summarizer", "target": "*",
    "capability": "llm",    "comms_mode": "mediated" }
]}
```

The web-search agent can reach HTTP. The summarizer can reach the LLM. Nothing else is permitted — not because we forgot to write a rule allowing it, but because the default is deny.

---

## [5:00–6:30] Secrets Are Not Configuration ⚡

In most frameworks, API keys arrive as environment variables or config files. They live in the process for as long as the process lives — which in a long-running orchestrator, is forever.

In WASM_AF, secrets are delivered through a separate interface scoped to a specific link between a specific component and a specific provider. The web-search agent's Brave Search API key:

```rust
// components/agents/web-search/src/lib.rs:124–134

let secret_handle = get_secret("brave_api_key").map_err(|e| AgentError {
    code: ErrorCode::Internal,
    message: format!("failed to retrieve brave_api_key secret: {:?}", e),
})?;
let api_key = match reveal_secret(&secret_handle) {
    SecretValue::String(s) => s,
    SecretValue::Bytes(b) => String::from_utf8(b)...?,
};
```

`get_secret` returns an opaque handle — the value isn't in memory yet. `reveal_secret` decrypts it from the host's secrets backend and loads it into the component's memory immediately before use.

When this component is destroyed after the step completes, that decrypted value is gone. The LLM provider's API key, the database password for a different agent, the orchestrator's NATS credentials — the web-search agent has *no interface* through which to reach any of them. There's no function to call. There's no memory to read.

This isn't access control on top of a shared credential store. The isolation is structural.

---

## [6:30–7:30] The WIT Contract Layer ⚡

One more thing worth showing. Every capability in WASM_AF is defined as a WIT interface — WebAssembly Interface Types. This is the typed contract between components.

```wit
// wit/wasm-af-llm.wit
interface inference {
    record completion-request {
        model: string,
        messages: list<message>,
        max-tokens: u32,
        temperature: option<f32>,
    }

    complete: func(req: completion-request)
               -> result<completion-response, string>;
}
```

An agent that imports `wasm-af:llm/inference` can call `complete`. It cannot call anything else on the LLM provider. It cannot see the provider's connection pool, API key, or routing table. The WIT interface **is** the API surface. There is no `__dict__`, no reflection, no `eval`.

For your framework: this is the answer to "how do I know what a third-party agent can actually do?" The WIT file tells you exactly. Not documentation. Not convention. The binary cannot call what the interface doesn't define.

---

## [7:30–8:45] Where This Fits With NeMo

I want to be honest about what this is and isn't.

NeMo Agent Toolkit is excellent at what it does: composing agents into pipelines, profiling token usage, connecting to tool libraries, managing conversation state. WASM_AF doesn't compete with that. It operates at a different layer.

The README is explicit about this: "these are different execution models, not different flavors of the same idea."

But there's an interesting integration story for v2. NeMo supports the A2A protocol — Agent-to-Agent. WASM_AF could expose its orchestrator as an A2A server. Your framework handles high-level coordination. You decide *what* the pipeline does. When a step needs hardened execution — an agent calling an external API with a real credential, an agent running a code interpreter, an agent touching production data — you delegate that step to WASM_AF. WASM_AF handles *how* it executes safely.

Your orchestrator stays in Python. The blast radius of a prompt injection stays inside a 115 KB WASM binary that gets destroyed when the step completes.

---

## [8:45–9:30] What's Left Open

A few open questions I'd love input on:

**Token budgets across fan-out.** The LLM provider is the natural enforcement point. But when a task fans out to five parallel subagents, how should the budget propagate? Divided evenly? First-come first-served? Does the policy engine own this?

**Recursive decomposition latency.** When a subagent decides it needs another subagent, that round-trips through the orchestrator. For deep pipelines this could add up. Alternate model: the orchestrator builds the full plan upfront and pre-allocates the entire pipeline before any step runs.

**Multi-tenancy.** wasmCloud lattices provide namespace isolation. One lattice per tenant, or shared lattice with link-level isolation? The current model is one lattice per deployment.

---

## [9:30–10:00] Close

The core bet is simple: **the execution model should make insecure behavior impossible, not just unlikely.**

Docker showed us that OS-level sandboxing is worth the operational complexity. WASM is that same idea at the instruction level — and for AI agents with real credentials and real side effects, I think the security properties justify the build complexity.

The code is at `github.com/jolucas/wasm-af`. The policy engine, the web-search agent, the orchestrator loop — it's all there. I'd genuinely love to hear how you think about the threat model for NeMo agents and whether the WIT interface layer is something that would be useful to you.

Thank you.

---

## Appendix: Quick Reference for Q&A

| Question | Answer |
|---|---|
| "Can WASM components access the GPU?" | Not yet natively. You'd route GPU calls through a capability provider — same pattern as HTTP or LLM. The provider holds the GPU context; the agent calls a typed WIT interface. |
| "How does this compare to gVisor/Firecracker?" | gVisor/Firecracker are VM-level sandboxes — heavier, slower cold starts. WASM operates at the bytecode level: sub-ms starts, 100 KB binaries, no kernel emulation needed. |
| "Can you write agents in Python?" | Yes — `componentize-py` compiles Python to WASM components. You lose some of the type-safety benefits of WIT at the Python layer, but the runtime isolation and capability model still apply. |
| "What about streaming LLM responses?" | Open question. Options: stream tokens through the orchestrator (adds latency), or pre-approve a direct wRPC link between a specific agent pair (policy-gated, faster). |
| "Does deny-by-default break anything?" | By design. `deploy/policies/default.json` shows the minimal rule set for the research pipeline. Adding capabilities is an explicit policy decision, not a configuration opt-out. |
