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
	return []taskstate.Step{
		{
			ID:        stepID(taskID, 1),
			AgentType: "email-read",
			InputKey:  stepID(taskID, 1) + ".input",
			OutputKey: stepID(taskID, 1) + ".output",
			Status:    taskstate.StepPending,
			Params:    map[string]string{"folder": "inbox"},
		},
		{
			ID:        stepID(taskID, 2),
			AgentType: "responder",
			InputKey:  stepID(taskID, 2) + ".input",
			OutputKey: stepID(taskID, 2) + ".output",
			Status:    taskstate.StepPending,
		},
		{
			ID:        stepID(taskID, 3),
			AgentType: "email-send",
			InputKey:  stepID(taskID, 3) + ".input",
			OutputKey: stepID(taskID, 3) + ".output",
			Status:    taskstate.StepPending,
			Params: map[string]string{
				"to":      ctx["reply_to"],
				"subject": ctx["reply_subject"],
				"body":    ctx["reply_body"],
			},
		},
	}, nil
}
