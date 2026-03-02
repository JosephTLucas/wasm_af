package main

import "github.com/jolucas/wasm-af/pkg/taskstate"

// SkillDemoBuilder creates a minimal plan for demonstrating individual skills
// without the overhead of memory retrieval or LLM-based response generation.
// Plan: router → [skill splice]. The skill step's output is the final result.
type SkillDemoBuilder struct{}

func (SkillDemoBuilder) BuildPlan(taskID string, _ map[string]string, _ *AgentRegistry, _ *Orchestrator) ([]taskstate.Step, error) {
	routerID := stepID(taskID, 1)

	return []taskstate.Step{
		{
			ID:        routerID,
			AgentType: "router",
			InputKey:  routerID + ".input",
			OutputKey: routerID + ".output",
			Status:    taskstate.StepPending,
		},
	}, nil
}

// ChatBuilder creates a conversational AI plan.
//
// The initial plan has four steps:
//  1. memory (get)    — retrieve conversation history for the user
//  2. router          — classify the user's message into a skill + params
//  3. responder       — produce the final user-facing response
//  4. memory (append) — persist the response back to conversation history
//
// Between steps 2 and 3, the scheduler may splice in a concrete skill step
// (web-search, shell, file-ops) based on the router's output and OPA policy.
// The splice rewires responder to depend on the new skill step.
type ChatBuilder struct{}

func (ChatBuilder) BuildPlan(taskID string, ctx map[string]string, _ *AgentRegistry, _ *Orchestrator) ([]taskstate.Step, error) {
	user := ctx["user"]
	if user == "" {
		user = "default"
	}
	memKey := "memory:" + user

	memGetID := stepID(taskID, 1)
	routerID := stepID(taskID, 2)
	responderID := stepID(taskID, 3)
	memAppendID := stepID(taskID, 4)

	ctx["result_key"] = responderID + ".output"

	return []taskstate.Step{
		{
			ID:        memGetID,
			AgentType: "memory",
			InputKey:  memGetID + ".input",
			OutputKey: memGetID + ".output",
			Status:    taskstate.StepPending,
			Params:    map[string]string{"op": "get", "key": memKey},
		},
		{
			ID:        routerID,
			AgentType: "router",
			InputKey:  routerID + ".input",
			OutputKey: routerID + ".output",
			Status:    taskstate.StepPending,
			DependsOn: []string{memGetID},
		},
		{
			ID:        responderID,
			AgentType: "responder",
			InputKey:  responderID + ".input",
			OutputKey: responderID + ".output",
			Status:    taskstate.StepPending,
			DependsOn: []string{routerID},
		},
		{
			ID:        memAppendID,
			AgentType: "memory",
			InputKey:  memAppendID + ".input",
			OutputKey: memAppendID + ".output",
			Status:    taskstate.StepPending,
			DependsOn: []string{responderID},
			Params:    map[string]string{"op": "append", "key": memKey},
		},
	}, nil
}
