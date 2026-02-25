package main

import (
	"fmt"
	"net/url"
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
}

func stepID(taskID string, n int) string {
	return fmt.Sprintf("%s-step-%d", taskID, n)
}

// ResearchBuilder creates a two-step plan: web-search → summarizer.
type ResearchBuilder struct{}

func (ResearchBuilder) BuildPlan(taskID string, _ map[string]string, _ *AgentRegistry, _ *Orchestrator) ([]taskstate.Step, error) {
	return []taskstate.Step{
		{
			ID:        stepID(taskID, 1),
			AgentType: "web-search",
			InputKey:  stepID(taskID, 1) + ".input",
			OutputKey: stepID(taskID, 1) + ".output",
			Status:    taskstate.StepPending,
		},
		{
			ID:        stepID(taskID, 2),
			AgentType: "summarizer",
			InputKey:  stepID(taskID, 2) + ".input",
			OutputKey: stepID(taskID, 2) + ".output",
			Status:    taskstate.StepPending,
		},
	}, nil
}

// FanOutSummarizerBuilder creates N parallel url-fetch steps (one per URL)
// followed by a summarizer step.
type FanOutSummarizerBuilder struct{}

func (FanOutSummarizerBuilder) BuildPlan(taskID string, ctx map[string]string, _ *AgentRegistry, orch *Orchestrator) ([]taskstate.Step, error) {
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
	for i, u := range urls {
		n := i + 1
		domain := extractDomain(u)
		step := taskstate.Step{
			ID:           stepID(taskID, n),
			AgentType:    "url-fetch",
			InputKey:     stepID(taskID, n) + ".input",
			OutputKey:    stepID(taskID, n) + ".output",
			Status:       taskstate.StepPending,
			Group:        "fetch",
			AllowedHosts: domain,
			Params:       map[string]string{"url": u},
		}
		if !orch.fetchDomainAllowed(domain) {
			step.Status = taskstate.StepDenied
			step.AllowedHosts = ""
			step.Error = fmt.Sprintf("domain %q is not in the server's URL fetch allow list", domain)
		}
		steps = append(steps, step)
	}

	sumN := len(urls) + 1
	steps = append(steps, taskstate.Step{
		ID:        stepID(taskID, sumN),
		AgentType: "summarizer",
		InputKey:  stepID(taskID, sumN) + ".input",
		OutputKey: stepID(taskID, sumN) + ".output",
		Status:    taskstate.StepPending,
	})

	return steps, nil
}

// IsolationTestBuilder creates a url-fetch step with a deliberate mismatch
// between AllowedHosts and the target URL, used to demonstrate per-instance
// capability scoping.
type IsolationTestBuilder struct{}

func (IsolationTestBuilder) BuildPlan(taskID string, ctx map[string]string, _ *AgentRegistry, orch *Orchestrator) ([]taskstate.Step, error) {
	restrictedTo := ctx["restricted_to"]
	fetchURL := ctx["fetch_url"]
	if restrictedTo == "" || fetchURL == "" {
		return nil, fmt.Errorf("isolation-test requires 'restricted_to' and 'fetch_url' in context")
	}
	fetchDomain := extractDomain(fetchURL)
	step := taskstate.Step{
		ID:           stepID(taskID, 1),
		AgentType:    "url-fetch",
		InputKey:     stepID(taskID, 1) + ".input",
		OutputKey:    stepID(taskID, 1) + ".output",
		Status:       taskstate.StepPending,
		AllowedHosts: restrictedTo,
		Params:       map[string]string{"url": fetchURL},
	}
	if !orch.fetchDomainAllowed(fetchDomain) {
		step.Status = taskstate.StepDenied
		step.Error = fmt.Sprintf("domain %q is not in the server's URL fetch allow list", fetchDomain)
	}
	return []taskstate.Step{step}, nil
}

func extractDomain(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return rawURL
	}
	return u.Hostname()
}
