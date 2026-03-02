package main

import (
	"fmt"
	"strconv"

	"github.com/jolucas/wasm-af/pkg/taskstate"
)

// EmailReplyBuilder creates an email-reply workflow:
//
//  1. email-read   — fetch the inbox
//  2. responder    — draft a reply (OPA gates this step with jailbreak detection
//     on the email-read output from prior_results)
//  3. email-send   — deliver the reply
//
// The jailbreak check is not a separate step — it is the OPA policy evaluation
// that fires before the responder step. If injection is detected in the target
// email, the responder is denied, the task fails, and email-send never runs.
type EmailReplyBuilder struct{}

func (EmailReplyBuilder) BuildPlan(taskID string, ctx map[string]string, _ *AgentRegistry, _ *Orchestrator) ([]taskstate.Step, error) {
	readID := stepID(taskID, 1)
	responderID := stepID(taskID, 2)
	sendID := stepID(taskID, 3)

	ctx["result_key"] = sendID + ".output"

	return []taskstate.Step{
		{
			ID:        readID,
			AgentType: "email-read",
			InputKey:  readID + ".input",
			OutputKey: readID + ".output",
			Status:    taskstate.StepPending,
			Params:    map[string]string{"folder": "inbox"},
		},
		{
			ID:        responderID,
			AgentType: "responder",
			InputKey:  responderID + ".input",
			OutputKey: responderID + ".output",
			Status:    taskstate.StepPending,
			DependsOn: []string{readID},
		},
		{
			ID:        sendID,
			AgentType: "email-send",
			InputKey:  sendID + ".input",
			OutputKey: sendID + ".output",
			Status:    taskstate.StepPending,
			DependsOn: []string{responderID},
			Params: map[string]string{
				"to":      ctx["reply_to"],
				"subject": ctx["reply_subject"],
				"body":    ctx["reply_body"],
			},
		},
	}, nil
}

// ReplyAllBuilder creates a parallel reply-to-all-emails workflow:
//
//	email-read → responder-0 → email-send-0
//	           → responder-1 → email-send-1
//	           → ...
//
// Each responder step gets reply_to_index in its params so the jailbreak
// policy can inspect each email independently. Branches run in parallel;
// a denied branch (jailbreak detected) does not kill healthy branches.
//
// Context keys:
//
//	email_count       — number of emails to process (default: 2)
//	reply_to_N        — recipient address for email N
//	reply_subject_N   — subject line for email N
//	reply_body_N      — reply body for email N
type ReplyAllBuilder struct{}

func (ReplyAllBuilder) BuildPlan(taskID string, ctx map[string]string, _ *AgentRegistry, _ *Orchestrator) ([]taskstate.Step, error) {
	emailCount := 2
	if v, ok := ctx["email_count"]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			emailCount = n
		}
	}

	readID := stepID(taskID, 1)
	steps := []taskstate.Step{
		{
			ID:        readID,
			AgentType: "email-read",
			InputKey:  readID + ".input",
			OutputKey: readID + ".output",
			Status:    taskstate.StepPending,
			Params:    map[string]string{"folder": "inbox"},
		},
	}

	for i := 0; i < emailCount; i++ {
		idx := strconv.Itoa(i)
		respID := fmt.Sprintf("%s-respond-%d", taskID, i)
		sendID := fmt.Sprintf("%s-send-%d", taskID, i)

		steps = append(steps, taskstate.Step{
			ID:        respID,
			AgentType: "responder",
			InputKey:  respID + ".input",
			OutputKey: respID + ".output",
			Status:    taskstate.StepPending,
			DependsOn: []string{readID},
			Params:    map[string]string{"reply_to_index": idx},
		})

		steps = append(steps, taskstate.Step{
			ID:        sendID,
			AgentType: "email-send",
			InputKey:  sendID + ".input",
			OutputKey: sendID + ".output",
			Status:    taskstate.StepPending,
			DependsOn: []string{respID},
			Params: map[string]string{
				"to":      ctx[fmt.Sprintf("reply_to_%d", i)],
				"subject": ctx[fmt.Sprintf("reply_subject_%d", i)],
				"body":    ctx[fmt.Sprintf("reply_body_%d", i)],
			},
		})
	}

	return steps, nil
}
