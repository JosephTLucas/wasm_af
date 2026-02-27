# Email Reply Pipeline — Sequence Diagrams

## Scenario A: Clean email (happy path)

```mermaid
sequenceDiagram
    participant Client as run.sh
    participant API as Orchestrator API
    participant Loop as runTask loop
    participant OPA as OPA Policy
    participant WASM as WASM Runtime

    Client->>API: POST /tasks type email-reply, reply_to_index 0
    API->>OPA: evaluateSubmitPolicy email-reply
    OPA-->>API: allow
    API->>Loop: EmailReplyBuilder plan: email-read, responder, email-send
    API-->>Client: 202 task_id

    Note over Loop: Step 0 email-read
    Loop->>OPA: evaluateStepPolicy email-read, prior_results empty
    OPA-->>Loop: allow
    Loop->>WASM: create email_read.wasm, execute, destroy
    WASM-->>Loop: emails: alice clean, bob clean, support injected
    Note over Loop: store in state.Results skill_output

    Note over Loop: Step 1 responder
    Loop->>OPA: evaluateStepPolicy responder, prior_results has skill_output
    Note over OPA: emails index 0 is alice clean - no pattern match
    OPA-->>Loop: allow
    Loop->>WASM: create responder.wasm, llm_complete, destroy
    WASM-->>Loop: response Draft reply

    Note over Loop: Step 2 email-send
    Loop->>OPA: evaluateStepPolicy email-send
    OPA-->>Loop: allow
    Loop->>WASM: create email_send.wasm, send_email host fn, destroy
    WASM-->>Loop: status sent

    Loop-->>API: task status completed
    Client->>API: GET /tasks/id
    API-->>Client: status completed with response and lifecycle
```

## Scenario B: Injected email (blocked)

```mermaid
sequenceDiagram
    participant Client as run.sh
    participant API as Orchestrator API
    participant Loop as runTask loop
    participant OPA as OPA Policy
    participant WASM as WASM Runtime

    Client->>API: POST /tasks type email-reply, reply_to_index 2
    API->>OPA: evaluateSubmitPolicy email-reply
    OPA-->>API: allow
    API->>Loop: EmailReplyBuilder plan: email-read, responder, email-send
    API-->>Client: 202 task_id

    Note over Loop: Step 0 email-read
    Loop->>OPA: evaluateStepPolicy email-read, prior_results empty
    OPA-->>Loop: allow
    Loop->>WASM: create email_read.wasm, execute, destroy
    WASM-->>Loop: emails: alice clean, bob clean, support injected
    Note over Loop: store in state.Results skill_output

    Note over Loop: Step 1 responder
    Loop->>OPA: evaluateStepPolicy responder, prior_results has skill_output
    Note over OPA: emails index 2 is support injected - pattern match!
    OPA-->>Loop: DENY jailbreak detected
    Loop->>Loop: mark step denied and failTask
    Note over Loop: Step 2 email-send NEVER RUNS

    Loop-->>API: task status failed
    Client->>API: GET /tasks/id
    API-->>Client: status failed with error and lifecycle
```
