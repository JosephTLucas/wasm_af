package main

import "github.com/jolucas/wasm-af/pkg/taskstate"

// ChatBuilder creates a conversational AI plan.
//
// The initial plan has four steps:
//  1. memory (get)    — retrieve conversation history for the user
//  2. router          — classify the user's message into a skill + params
//  3. responder       — produce the final user-facing response
//  4. memory (append) — persist the response back to conversation history
//
// Between steps 2 and 3, the runTask loop may splice in a concrete skill step
// (web-search, shell, file-ops) based on the router's output and OPA policy.
type ChatBuilder struct{}

func (ChatBuilder) BuildPlan(taskID string, ctx map[string]string, _ *AgentRegistry, _ *Orchestrator) ([]taskstate.Step, error) {
	user := ctx["user"]
	if user == "" {
		user = "default"
	}
	memKey := "memory:" + user

	return []taskstate.Step{
		{
			ID:        stepID(taskID, 1),
			AgentType: "memory",
			InputKey:  stepID(taskID, 1) + ".input",
			OutputKey: stepID(taskID, 1) + ".output",
			Status:    taskstate.StepPending,
			Params:    map[string]string{"op": "get", "key": memKey},
		},
		{
			ID:        stepID(taskID, 2),
			AgentType: "router",
			InputKey:  stepID(taskID, 2) + ".input",
			OutputKey: stepID(taskID, 2) + ".output",
			Status:    taskstate.StepPending,
		},
		{
			ID:        stepID(taskID, 3),
			AgentType: "responder",
			InputKey:  stepID(taskID, 3) + ".input",
			OutputKey: stepID(taskID, 3) + ".output",
			Status:    taskstate.StepPending,
		},
		{
			ID:        stepID(taskID, 4),
			AgentType: "memory",
			InputKey:  stepID(taskID, 4) + ".input",
			OutputKey: stepID(taskID, 4) + ".output",
			Status:    taskstate.StepPending,
			// value is intentionally empty; memory agent picks it from context["response"].
			Params: map[string]string{"op": "append", "key": memKey},
		},
	}, nil
}
