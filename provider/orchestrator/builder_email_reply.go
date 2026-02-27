package main

import "github.com/jolucas/wasm-af/pkg/taskstate"

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
