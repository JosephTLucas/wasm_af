package main

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	wrpc "wrpc.io/go"
	wrpcnats "wrpc.io/go/nats"

	"github.com/jolucas/wasm-af/pkg/controlplane"
	"github.com/jolucas/wasm-af/pkg/taskstate"
)

// PolicyCapability mirrors the WIT wasm-af:policy/evaluator capability enum.
type PolicyCapability string

const (
	CapHTTP        PolicyCapability = "http"
	CapLLM         PolicyCapability = "llm"
	CapKV          PolicyCapability = "kv"
	CapAgentDirect PolicyCapability = "agent-direct"
)

// CommsMode mirrors the WIT wasm-af:policy/evaluator comms-mode enum.
type CommsMode string

const (
	CommsModeMediated CommsMode = "mediated"
	CommsModeDirected CommsMode = "direct"
)

// policyEvalRequest holds the fields of the WIT link-request record.
type policyEvalRequest struct {
	SourceComponentID string
	TargetComponentID string
	Capability        PolicyCapability
	TaskID            string
}

// capabilityDisc returns the WIT enum discriminant for a PolicyCapability.
// Order matches the WIT declaration: http=0, llm=1, kv=2, agent-direct=3.
func capabilityDisc(cap PolicyCapability) byte {
	switch cap {
	case CapHTTP:
		return 0
	case CapLLM:
		return 1
	case CapKV:
		return 2
	case CapAgentDirect:
		return 3
	default:
		return 0
	}
}

// EvaluateLink calls the policy engine component via wRPC and returns the approved
// CommsMode, or an error if the link is denied or the call fails.
//
// Every call — permit or deny — is appended to the task audit log.
func (o *Orchestrator) EvaluateLink(
	ctx context.Context,
	taskID, stepID string,
	source, target string,
	cap PolicyCapability,
) (CommsMode, error) {
	ctx, span := tracer.Start(ctx, "orchestrator.evaluate_policy")
	defer span.End()

	span.SetAttributes(
		attribute.String("policy.source", source),
		attribute.String("policy.target", target),
		attribute.String("policy.capability", string(cap)),
		attribute.String("task.id", taskID),
	)

	// Invoke the policy engine via wRPC. The policy engine component is linked
	// to the orchestrator provider and reachable through the wRPC client.
	mode, denyCode, denyMsg, err := o.callPolicyEngine(ctx, policyEvalRequest{
		SourceComponentID: source,
		TargetComponentID: target,
		Capability:        cap,
		TaskID:            taskID,
	})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return "", fmt.Errorf("policy engine call failed: %w", err)
	}

	if denyCode != "" {
		// Log the denial and return an error so the orchestrator can fail the step.
		_ = o.store.AppendAudit(ctx, &taskstate.AuditEvent{
			TaskID:           taskID,
			StepID:           stepID,
			EventType:        taskstate.EventPolicyDeny,
			PolicySource:     source,
			PolicyTarget:     target,
			PolicyCapability: string(cap),
			PolicyDenyCode:   denyCode,
			PolicyDenyMsg:    denyMsg,
			Timestamp:        time.Now().UTC(),
		})
		span.SetStatus(codes.Error, denyMsg)
		return "", fmt.Errorf("policy denied: [%s] %s", denyCode, denyMsg)
	}

	_ = o.store.AppendAudit(ctx, &taskstate.AuditEvent{
		TaskID:           taskID,
		StepID:           stepID,
		EventType:        taskstate.EventPolicyPermit,
		PolicySource:     source,
		PolicyTarget:     target,
		PolicyCapability: string(cap),
		PolicyCommsMode:  string(mode),
		Timestamp:        time.Now().UTC(),
	})

	return mode, nil
}

// callPolicyEngine invokes wasm-af:policy/evaluator.evaluate on the policy engine
// component via the wRPC NATS transport with component model canonical ABI encoding.
//
// Returns (commsMode, "", "", nil) on permit.
// Returns ("", denyCode, denyMsg, nil) on deny.
// Returns ("", "", "", err) on transport or encoding error.
func (o *Orchestrator) callPolicyEngine(
	ctx context.Context,
	req policyEvalRequest,
) (mode CommsMode, denyCode, denyMsg string, err error) {
	// Create a wRPC NATS client targeting the policy engine component.
	// Subject prefix format: {lattice}.{componentID}
	// Invocation subject: {prefix}.wrpc.0.0.1.{instance}.{name}
	client := wrpcnats.NewClient(o.nats,
		wrpcnats.WithPrefix(fmt.Sprintf("%s.%s", o.lattice, o.policyEngineID)),
	)

	// Encode link-request record using the component model canonical ABI.
	// Field order matches the WIT declaration:
	//   source-component-id: string
	//   target-component-id: string
	//   capability: capability  (u8 discriminant)
	//   task-id: string
	var buf bytes.Buffer
	if err := wrpc.WriteString(req.SourceComponentID, &buf); err != nil {
		return "", "", "", fmt.Errorf("encode source-component-id: %w", err)
	}
	if err := wrpc.WriteString(req.TargetComponentID, &buf); err != nil {
		return "", "", "", fmt.Errorf("encode target-component-id: %w", err)
	}
	if err := buf.WriteByte(capabilityDisc(req.Capability)); err != nil {
		return "", "", "", fmt.Errorf("encode capability: %w", err)
	}
	if err := wrpc.WriteString(req.TaskID, &buf); err != nil {
		return "", "", "", fmt.Errorf("encode task-id: %w", err)
	}

	invokeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	w, r, err := client.Invoke(invokeCtx,
		"wasm-af:policy/evaluator@0.1.0", "evaluate", buf.Bytes())
	if err != nil {
		return "", "", "", fmt.Errorf("wRPC policy invocation: %w", err)
	}
	defer r.Close()
	// All parameters were in buf; no async writes needed.
	if cErr := w.Close(); cErr != nil {
		o.logger.Warn("failed to close policy wRPC param writer", "err", cErr)
	}

	// Decode result<permit, deny-reason>.
	// result discriminant: 0 = ok, 1 = err  (single byte)
	isOk, err := wrpc.ReadResultStatus(r)
	if err != nil {
		return "", "", "", fmt.Errorf("read result status: %w", err)
	}

	if isOk {
		// permit record: comms-mode (u8 enum: mediated=0, direct=1)
		b, err := r.ReadByte()
		if err != nil {
			return "", "", "", fmt.Errorf("read comms-mode: %w", err)
		}
		switch b {
		case 0:
			return CommsModeMediated, "", "", nil
		case 1:
			return CommsModeDirected, "", "", nil
		default:
			return "", "", "", fmt.Errorf("unknown comms-mode discriminant %d", b)
		}
	}

	// deny-reason record:
	//   code: deny-code  (u8 enum: not-allowed=0, capability-not-permitted=1, policy-config-error=2)
	//   message: string
	codeByte, err := r.ReadByte()
	if err != nil {
		return "", "", "", fmt.Errorf("read deny-code: %w", err)
	}
	msg, err := wrpc.ReadString(r)
	if err != nil {
		return "", "", "", fmt.Errorf("read deny-message: %w", err)
	}

	denyCodeStr := map[byte]string{
		0: "not-allowed",
		1: "capability-not-permitted",
		2: "policy-config-error",
	}[codeByte]
	if denyCodeStr == "" {
		denyCodeStr = fmt.Sprintf("unknown-deny-code-%d", codeByte)
	}

	return "", denyCodeStr, msg, nil
}

// linkForStep builds the LinkDefinition for a given agent type → target pair.
// For HTTP links it populates SourceConfig.allowed_hosts from the orchestrator's
// per-agent allow-list configuration, enforcing network boundaries at the link level.
func (o *Orchestrator) linkForStep(agentType, agentComponentID, targetID string, cap PolicyCapability) controlplane.LinkDefinition {
	witNS, witPkg, ifaces := witInterfaceForCap(cap)
	link := controlplane.LinkDefinition{
		SourceID:     agentComponentID,
		Target:       targetID,
		TargetType:   targetTypeForCap(cap),
		WitNamespace: witNS,
		WitPackage:   witPkg,
		Interfaces:   ifaces,
	}
	// Attach the domain allow-list to HTTP capability links so that any
	// wasi:http/outgoing-handler provider that honours SourceConfig.allowed_hosts
	// will enforce the restriction at the network level.
	if cap == CapHTTP {
		if hosts, ok := o.allowedHosts[agentType]; ok && hosts != "" {
			link.SourceConfig = map[string]string{"allowed_hosts": hosts}
		}
	}
	return link
}

func witInterfaceForCap(cap PolicyCapability) (ns, pkg string, ifaces []string) {
	switch cap {
	case CapHTTP:
		return "wasi", "http", []string{"outgoing-handler"}
	case CapLLM:
		return "wasm-af", "llm", []string{"inference"}
	case CapKV:
		return "wasi", "keyvalue", []string{"store"}
	case CapAgentDirect:
		return "wasm-af", "agent", []string{"peer"}
	default:
		return "unknown", "unknown", nil
	}
}

func targetTypeForCap(cap PolicyCapability) controlplane.TargetType {
	if cap == CapAgentDirect {
		return controlplane.TargetComponent
	}
	return controlplane.TargetProvider
}
