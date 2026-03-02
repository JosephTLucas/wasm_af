package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jolucas/wasm-af/pkg/taskstate"
)

// PlanBuilder creates an execution plan ([]Step) for a given task type.
type PlanBuilder interface {
	BuildPlan(taskID string, ctx map[string]string, registry *AgentRegistry, orch *Orchestrator) ([]taskstate.Step, error)
}

// PlanBuilderRegistry maps task type names to PlanBuilder implementations.
type PlanBuilderRegistry struct {
	builders map[string]PlanBuilder
}

// NewPlanBuilderRegistry creates an empty registry.
func NewPlanBuilderRegistry() *PlanBuilderRegistry {
	return &PlanBuilderRegistry{builders: make(map[string]PlanBuilder)}
}

// Register adds a builder for a task type.
func (r *PlanBuilderRegistry) Register(name string, b PlanBuilder) {
	r.builders[name] = b
}

// Build dispatches to the registered builder for taskType.
func (r *PlanBuilderRegistry) Build(taskType, taskID string, ctx map[string]string, registry *AgentRegistry, orch *Orchestrator) ([]taskstate.Step, error) {
	b, ok := r.builders[taskType]
	if !ok {
		return nil, fmt.Errorf("unknown task type %q", taskType)
	}
	return b.BuildPlan(taskID, ctx, registry, orch)
}

// RegisterDefaultBuilders registers the built-in task type builders.
func RegisterDefaultBuilders(r *PlanBuilderRegistry) {
	r.Register("research", ResearchBuilder{})
	r.Register("fan-out-summarizer", FanOutSummarizerBuilder{})
	r.Register("isolation-test", IsolationTestBuilder{})
	r.Register("generic", GenericPlanBuilder{})
}

func stepID(taskID string, n int) string {
	return fmt.Sprintf("%s-step-%d", taskID, n)
}

// ResearchBuilder creates a two-step plan: web-search → summarizer.
type ResearchBuilder struct{}

func (ResearchBuilder) BuildPlan(taskID string, ctx map[string]string, _ *AgentRegistry, _ *Orchestrator) ([]taskstate.Step, error) {
	searchID := stepID(taskID, 1)
	sumID := stepID(taskID, 2)

	ctx["result_key"] = sumID + ".output"

	return []taskstate.Step{
		{
			ID:        searchID,
			AgentType: "web-search",
			InputKey:  searchID + ".input",
			OutputKey: searchID + ".output",
			Status:    taskstate.StepPending,
		},
		{
			ID:        sumID,
			AgentType: "summarizer",
			InputKey:  sumID + ".input",
			OutputKey: sumID + ".output",
			Status:    taskstate.StepPending,
			DependsOn: []string{searchID},
		},
	}, nil
}

// FanOutSummarizerBuilder creates N parallel url-fetch steps (one per URL)
// followed by a summarizer step.
type FanOutSummarizerBuilder struct{}

func (FanOutSummarizerBuilder) BuildPlan(taskID string, ctx map[string]string, _ *AgentRegistry, _ *Orchestrator) ([]taskstate.Step, error) {
	raw, ok := ctx["urls"]
	if !ok || raw == "" {
		return nil, fmt.Errorf("fan-out-summarizer requires a comma-separated 'urls' key in context")
	}

	var urls []string
	for _, u := range strings.Split(raw, ",") {
		u = strings.TrimSpace(u)
		if u != "" {
			urls = append(urls, u)
		}
	}
	if len(urls) == 0 {
		return nil, fmt.Errorf("fan-out-summarizer: no valid urls provided")
	}

	steps := make([]taskstate.Step, 0, len(urls)+1)
	fetchIDs := make([]string, 0, len(urls))
	for i, u := range urls {
		n := i + 1
		id := stepID(taskID, n)
		fetchIDs = append(fetchIDs, id)
		steps = append(steps, taskstate.Step{
			ID:        id,
			AgentType: "url-fetch",
			InputKey:  id + ".input",
			OutputKey: id + ".output",
			Status:    taskstate.StepPending,
			Params:    map[string]string{"url": u},
		})
	}

	sumID := stepID(taskID, len(urls)+1)
	steps = append(steps, taskstate.Step{
		ID:        sumID,
		AgentType: "summarizer",
		InputKey:  sumID + ".input",
		OutputKey: sumID + ".output",
		Status:    taskstate.StepPending,
		DependsOn: fetchIDs,
	})

	ctx["result_key"] = sumID + ".output"

	return steps, nil
}

// IsolationTestBuilder creates a url-fetch step with a deliberate mismatch
// between the policy-assigned allowed_hosts and the target URL, used to
// demonstrate per-instance capability scoping. The "restricted_to" param
// is read by the Rego policy to set allowed_hosts.
type IsolationTestBuilder struct{}

func (IsolationTestBuilder) BuildPlan(taskID string, ctx map[string]string, _ *AgentRegistry, _ *Orchestrator) ([]taskstate.Step, error) {
	restrictedTo := ctx["restricted_to"]
	fetchURL := ctx["fetch_url"]
	if restrictedTo == "" || fetchURL == "" {
		return nil, fmt.Errorf("isolation-test requires 'restricted_to' and 'fetch_url' in context")
	}
	step := taskstate.Step{
		ID:        stepID(taskID, 1),
		AgentType: "url-fetch",
		InputKey:  stepID(taskID, 1) + ".input",
		OutputKey: stepID(taskID, 1) + ".output",
		Status:    taskstate.StepPending,
		Params:    map[string]string{"url": fetchURL, "restricted_to": restrictedTo},
	}
	return []taskstate.Step{step}, nil
}

// GenericPlanBuilder constructs a plan from a JSON array of step definitions
// passed in the task submission context under the "steps" key. This allows
// arbitrary workflows to be submitted without writing Go code.
type GenericPlanBuilder struct{}

func (GenericPlanBuilder) BuildPlan(taskID string, ctx map[string]string, _ *AgentRegistry, _ *Orchestrator) ([]taskstate.Step, error) {
	raw, ok := ctx["steps"]
	if !ok {
		return nil, fmt.Errorf("generic plan requires a 'steps' key in context (JSON array)")
	}
	var stepDefs []struct {
		AgentType string            `json:"agent_type"`
		DependsOn []string          `json:"depends_on,omitempty"`
		Params    map[string]string `json:"params,omitempty"`
	}
	if err := json.Unmarshal([]byte(raw), &stepDefs); err != nil {
		return nil, fmt.Errorf("parse steps: %w", err)
	}
	if len(stepDefs) == 0 {
		return nil, fmt.Errorf("generic plan: steps array is empty")
	}

	// Build an ID map so depends_on can reference 1-based step indices ("step-N")
	// or the auto-generated full IDs.
	steps := make([]taskstate.Step, len(stepDefs))
	for i, sd := range stepDefs {
		n := i + 1
		steps[i] = taskstate.Step{
			ID:        stepID(taskID, n),
			AgentType: sd.AgentType,
			InputKey:  stepID(taskID, n) + ".input",
			OutputKey: stepID(taskID, n) + ".output",
			Status:    taskstate.StepPending,
			DependsOn: sd.DependsOn,
			Params:    sd.Params,
		}
	}
	return steps, nil
}
