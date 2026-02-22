package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	nats "github.com/nats-io/nats.go"
	natsjetstream "github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.wasmcloud.dev/provider"
	wrpc "wrpc.io/go"
	wrpcnats "wrpc.io/go/nats"

	"github.com/jolucas/wasm-af/pkg/controlplane"
	"github.com/jolucas/wasm-af/pkg/taskstate"
)

// Orchestrator is the central coordinator. It holds references to the task store,
// control plane client, and the wasmCloud provider SDK instance.
type Orchestrator struct {
	logger         *slog.Logger
	store          *taskstate.Store
	ctl            *controlplane.Client
	js             natsjetstream.JetStream
	nats           *nats.Conn
	lattice        string
	policyEngineID string            // wasmCloud component ID of the policy engine
	llmProviderID  string            // wasmCloud provider key of the LLM inference provider
	httpProviderID string            // wasmCloud provider key of the HTTP client provider
	agentRefs      map[string]string // agent type → OCI image reference
	allowedHosts   map[string]string // agent type → comma-separated allowed hosts for HTTP links
}

// providerIDForCap returns the wasmCloud component/provider ID for the capability.
func (o *Orchestrator) providerIDForCap(cap PolicyCapability) string {
	switch cap {
	case CapHTTP:
		return o.httpProviderID
	case CapLLM:
		return o.llmProviderID
	case CapKV:
		return "wasm-af-kv-provider"
	default:
		return ""
	}
}

// agentKVPair mirrors the WIT kv-pair record used in task-input.context and
// task-output.metadata. JSON tags are used when building JSON payloads in loop.go.
type agentKVPair struct {
	Key string `json:"key"`
	Val string `json:"val"`
}

// invokeAgent calls wasm-af:agent/handler.execute on the given component via wRPC
// using component model canonical ABI encoding. Parameters are encoded directly
// into the initial wRPC invocation buffer; no async parameter streams are needed.
func (o *Orchestrator) invokeAgent(
	ctx context.Context,
	componentID, taskID, stepID, inputPayload string,
	inputContext []agentKVPair,
) (string, error) {
	ctx, span := tracer.Start(ctx, "orchestrator.invoke_agent")
	defer span.End()
	span.SetAttributes(
		attribute.String("agent.component_id", componentID),
		attribute.String("task.id", taskID),
		attribute.String("step.id", stepID),
	)

	// Create a wRPC NATS client targeting the agent component.
	client := wrpcnats.NewClient(o.nats,
		wrpcnats.WithPrefix(fmt.Sprintf("%s.%s", o.lattice, componentID)),
	)

	// Encode task-input record using the component model canonical ABI.
	// Field order matches the WIT declaration:
	//   task-id: string
	//   step-id: string
	//   payload: string
	//   context: list<kv-pair>  (each kv-pair: key string, val string)
	var buf bytes.Buffer
	if err := wrpc.WriteString(taskID, &buf); err != nil {
		return "", fmt.Errorf("encode task-id: %w", err)
	}
	if err := wrpc.WriteString(stepID, &buf); err != nil {
		return "", fmt.Errorf("encode step-id: %w", err)
	}
	if err := wrpc.WriteString(inputPayload, &buf); err != nil {
		return "", fmt.Errorf("encode payload: %w", err)
	}
	if err := wrpc.WriteList(inputContext, &buf, func(kv agentKVPair, w wrpc.ByteWriter) error {
		if err := wrpc.WriteString(kv.Key, w); err != nil {
			return err
		}
		return wrpc.WriteString(kv.Val, w)
	}); err != nil {
		return "", fmt.Errorf("encode context: %w", err)
	}

	invokeCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	w, r, err := client.Invoke(invokeCtx,
		"wasm-af:agent/handler@0.1.0", "execute", buf.Bytes())
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return "", fmt.Errorf("wRPC agent invocation: %w", err)
	}
	defer r.Close()
	if cErr := w.Close(); cErr != nil {
		o.logger.Warn("failed to close agent wRPC param writer", "err", cErr)
	}

	// Decode result<task-output, agent-error>.
	// result discriminant: 0 = ok, 1 = err  (single byte)
	isOk, err := wrpc.ReadResultStatus(r)
	if err != nil {
		return "", fmt.Errorf("read result status: %w", err)
	}

	if isOk {
		// task-output record:
		//   payload: string
		//   metadata: list<kv-pair>  (drain; not stored here)
		payload, err := wrpc.ReadString(r)
		if err != nil {
			return "", fmt.Errorf("read task-output payload: %w", err)
		}
		// drain metadata list so the reader is fully consumed
		if err := drainKVList(r); err != nil {
			o.logger.Warn("failed to drain task-output metadata", "err", err)
		}
		span.SetStatus(codes.Ok, "agent completed")
		return payload, nil
	}

	// agent-error record:
	//   code: error-code  (u8 enum: invalid-input=0, capability-failure=1, timeout=2, internal=3)
	//   message: string
	codeByte, err := r.ReadByte()
	if err != nil {
		return "", fmt.Errorf("read agent error code: %w", err)
	}
	msg, err := wrpc.ReadString(r)
	if err != nil {
		return "", fmt.Errorf("read agent error message: %w", err)
	}
	errorCodeStr := map[byte]string{
		0: "invalid-input",
		1: "capability-failure",
		2: "timeout",
		3: "internal",
	}[codeByte]
	if errorCodeStr == "" {
		errorCodeStr = fmt.Sprintf("unknown-error-code-%d", codeByte)
	}
	agentErr := fmt.Errorf("[%s] %s", errorCodeStr, msg)
	span.RecordError(agentErr)
	span.SetStatus(codes.Error, agentErr.Error())
	return "", agentErr
}

// drainKVList reads and discards a list<kv-pair> from r.
func drainKVList(r wrpc.IndexReadCloser) error {
	n, err := wrpc.ReadUint32(r)
	if err != nil {
		return err
	}
	for i := uint32(0); i < n; i++ {
		if _, err := wrpc.ReadString(r); err != nil {
			return err
		}
		if _, err := wrpc.ReadString(r); err != nil {
			return err
		}
	}
	return nil
}

// initFromHostConfig reads static orchestrator configuration from the wasmCloud
// host data config map (populated from WADM named config at provider startup).
//
// Keys:
//   - policy_engine_id, llm_provider_id, http_provider_id
//   - agent.<type>=<oci-ref>                    → OCI reference for agent type
//   - agent.<type>.allowed_hosts=<host,...>     → comma-separated HTTP allow-list
func (o *Orchestrator) initFromHostConfig(cfg map[string]string) {
	if id, ok := cfg["policy_engine_id"]; ok && id != "" {
		o.policyEngineID = id
	}
	if id, ok := cfg["llm_provider_id"]; ok && id != "" {
		o.llmProviderID = id
	}
	if id, ok := cfg["http_provider_id"]; ok && id != "" {
		o.httpProviderID = id
	}
	for k, v := range cfg {
		if !strings.HasPrefix(k, "agent.") {
			continue
		}
		rest := k[len("agent."):]
		// agent.<type>.allowed_hosts has priority; check it before the plain ref.
		if strings.HasSuffix(rest, ".allowed_hosts") {
			agentType := rest[:len(rest)-len(".allowed_hosts")]
			if agentType != "" {
				o.allowedHosts[agentType] = v
			}
		} else {
			// agent.<type>=<oci-ref>
			o.agentRefs[rest] = v
		}
	}
	o.logger.Info("orchestrator config loaded",
		"policy_engine_id", o.policyEngineID,
		"llm_provider_id", o.llmProviderID,
		"http_provider_id", o.httpProviderID,
		"agent_refs", o.agentRefs,
		"allowed_hosts", o.allowedHosts,
	)
}

// initFromLinkConfig handles dynamic agent registration when a component links
// to the orchestrator at runtime. The source component can advertise its OCI
// ref via link source config keys of the form "agent.<type>=<oci-ref>".
func (o *Orchestrator) initFromLinkConfig(link provider.InterfaceLinkDefinition) {
	for k, v := range link.SourceConfig {
		if len(k) > 6 && k[:6] == "agent." {
			agentType := k[6:]
			if o.agentRefs == nil {
				o.agentRefs = make(map[string]string)
			}
			o.agentRefs[agentType] = v
			o.logger.Info("registered agent ref from link", "type", agentType, "ref", v)
		}
	}
}
