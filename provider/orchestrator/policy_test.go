package main

import (
	"context"
	"testing"

	extism "github.com/extism/go-sdk"
)

const testStepPolicy = `package wasm_af.authz

default allow := false

allow if {
	input.step.agent_type == "url-fetch"
	input.agent.capability == "http"
	input.step.domain in data.config.allowed_domains
}

allow if {
	input.step.agent_type == "summarizer"
	input.agent.capability == "llm"
}

allow if {
	input.step.agent_type == "web-search"
	input.agent.capability == "http"
}

allow if {
	input.step.agent_type == "code-runner"
	input.agent.capability == "exec"
}

max_memory_pages := 64 if { input.step.agent_type == "url-fetch" }
max_memory_pages := 256 if { input.step.agent_type == "summarizer" }

allowed_hosts := [input.step.domain] if {
	input.step.agent_type == "url-fetch"
	input.step.domain
}

host_functions := ["llm_complete"] if {
	input.step.agent_type == "summarizer"
}

config := {"api_key": "secret-from-policy"} if {
	input.step.agent_type == "web-search"
}

allowed_paths := {"/tmp/workspace": "/sandbox"} if {
	input.step.agent_type == "code-runner"
}

deny_message := msg if {
	not allow
	msg := sprintf("no rule permits %s (%s)", [input.step.agent_type, input.agent.capability])
}
`

const testSubmitPolicy = `package wasm_af.submit

default allow := false

allow if {
	input.task_type in data.config.allowed_task_types
}

deny_message := msg if {
	not allow
	msg := sprintf("task type %q is not allowed", [input.task_type])
}
`

func testData() map[string]any {
	return map[string]any{
		"config": map[string]any{
			"allowed_domains":    []string{"webassembly.org", "wasmcloud.com"},
			"allowed_task_types": []string{"fan-out-summarizer", "research", "generic"},
		},
	}
}

func newTestEvaluator(t *testing.T) *OPAEvaluator {
	t.Helper()
	modules := map[string]string{
		"authz.rego":  testStepPolicy,
		"submit.rego": testSubmitPolicy,
	}
	e, err := NewOPAEvaluator(context.Background(), modules, testData())
	if err != nil {
		t.Fatalf("NewOPAEvaluator: %v", err)
	}
	return e
}

func stepInput(agentType, capability, domain string) map[string]any {
	step := map[string]any{
		"agent_type": agentType,
		"params":     map[string]string{},
	}
	if domain != "" {
		step["domain"] = domain
	}
	return map[string]any{
		"step":  step,
		"agent": map[string]any{"capability": capability},
		"task":  map[string]any{"id": "t1", "type": "test"},
		"plan":  map[string]any{"total_steps": 1, "completed_steps": 0},
	}
}

func TestOPA_RichInput(t *testing.T) {
	e := newTestEvaluator(t)

	r, err := e.EvaluateStep(context.Background(), stepInput("url-fetch", "http", "webassembly.org"))
	if err != nil {
		t.Fatal(err)
	}
	if !r.Permitted {
		t.Error("expected permit for url-fetch/webassembly.org")
	}
}

func TestOPA_DataDrivenDomainAllow(t *testing.T) {
	e := newTestEvaluator(t)

	for _, domain := range []string{"webassembly.org", "wasmcloud.com"} {
		r, err := e.EvaluateStep(context.Background(), stepInput("url-fetch", "http", domain))
		if err != nil {
			t.Fatal(err)
		}
		if !r.Permitted {
			t.Errorf("expected permit for url-fetch/%s", domain)
		}
	}
}

func TestOPA_DataDrivenDomainDeny(t *testing.T) {
	e := newTestEvaluator(t)

	r, err := e.EvaluateStep(context.Background(), stepInput("url-fetch", "http", "evil.com"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Permitted {
		t.Error("expected deny for evil.com")
	}
	if r.DenyMessage == nil {
		t.Error("expected deny_message")
	}
}

func TestOPA_UpdateData(t *testing.T) {
	e := newTestEvaluator(t)

	r, err := e.EvaluateStep(context.Background(), stepInput("url-fetch", "http", "example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Permitted {
		t.Fatal("expected deny for example.com before update")
	}

	if err := e.UpdateData(context.Background(), "/config/allowed_domains", []string{"example.com"}); err != nil {
		t.Fatalf("UpdateData: %v", err)
	}

	r, err = e.EvaluateStep(context.Background(), stepInput("url-fetch", "http", "example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if !r.Permitted {
		t.Error("expected permit for example.com after update")
	}
}

func TestOPA_StructuredDecisions_AllowedHosts(t *testing.T) {
	e := newTestEvaluator(t)

	r, err := e.EvaluateStep(context.Background(), stepInput("url-fetch", "http", "webassembly.org"))
	if err != nil {
		t.Fatal(err)
	}
	if !r.Permitted {
		t.Fatal("expected permit")
	}
	if len(r.AllowedHosts) != 1 || r.AllowedHosts[0] != "webassembly.org" {
		t.Errorf("expected allowed_hosts=[webassembly.org], got %v", r.AllowedHosts)
	}
}

func TestOPA_StructuredDecisions_MaxMemPages(t *testing.T) {
	e := newTestEvaluator(t)

	r, err := e.EvaluateStep(context.Background(), stepInput("url-fetch", "http", "webassembly.org"))
	if err != nil {
		t.Fatal(err)
	}
	if r.MaxMemPages == nil || *r.MaxMemPages != 64 {
		t.Errorf("expected max_memory_pages=64 for url-fetch, got %v", r.MaxMemPages)
	}

	r, err = e.EvaluateStep(context.Background(), stepInput("summarizer", "llm", ""))
	if err != nil {
		t.Fatal(err)
	}
	if r.MaxMemPages == nil || *r.MaxMemPages != 256 {
		t.Errorf("expected max_memory_pages=256 for summarizer, got %v", r.MaxMemPages)
	}
}

func TestOPA_StructuredDecisions_HostFunctions(t *testing.T) {
	e := newTestEvaluator(t)

	r, err := e.EvaluateStep(context.Background(), stepInput("summarizer", "llm", ""))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.HostFunctions) != 1 || r.HostFunctions[0] != "llm_complete" {
		t.Errorf("expected host_functions=[llm_complete], got %v", r.HostFunctions)
	}
}

func TestOPA_StructuredDecisions_Config(t *testing.T) {
	e := newTestEvaluator(t)

	r, err := e.EvaluateStep(context.Background(), stepInput("web-search", "http", ""))
	if err != nil {
		t.Fatal(err)
	}
	if r.Config == nil || r.Config["api_key"] != "secret-from-policy" {
		t.Errorf("expected config.api_key=secret-from-policy, got %v", r.Config)
	}
}

func TestOPA_StructuredDecisions_AllowedPaths(t *testing.T) {
	e := newTestEvaluator(t)

	r, err := e.EvaluateStep(context.Background(), stepInput("code-runner", "exec", ""))
	if err != nil {
		t.Fatal(err)
	}
	if r.AllowedPaths == nil || r.AllowedPaths["/tmp/workspace"] != "/sandbox" {
		t.Errorf("expected allowed_paths, got %v", r.AllowedPaths)
	}
}

func TestOPA_SubmitPolicy(t *testing.T) {
	e := newTestEvaluator(t)

	r, err := e.EvaluateSubmit(context.Background(), map[string]any{
		"task_type": "fan-out-summarizer",
		"query":     "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Permitted {
		t.Error("expected permit for fan-out-summarizer")
	}

	r, err = e.EvaluateSubmit(context.Background(), map[string]any{
		"task_type": "dangerous-task",
		"query":     "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Permitted {
		t.Error("expected deny for dangerous-task")
	}
}

func TestOPA_SubmitPolicyMissing(t *testing.T) {
	modules := map[string]string{
		"authz.rego": testStepPolicy,
	}
	e, err := NewOPAEvaluator(context.Background(), modules, testData())
	if err != nil {
		t.Fatalf("NewOPAEvaluator: %v", err)
	}

	r, err := e.EvaluateSubmit(context.Background(), map[string]any{
		"task_type": "anything",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Permitted {
		t.Error("expected default allow when submit package is absent")
	}
}

func TestOPA_NilEvaluator(t *testing.T) {
	o := &Orchestrator{}

	r, err := o.evaluateSubmitPolicy(context.Background(), "test", "q", nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Permitted {
		t.Error("expected deny for nil policy submit (fail closed)")
	}
}

func TestOPA_DenyUnknownAgent(t *testing.T) {
	e := newTestEvaluator(t)

	r, err := e.EvaluateStep(context.Background(), stepInput("evil-agent", "http", "webassembly.org"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Permitted {
		t.Error("expected deny for unknown agent")
	}
}

func TestOPA_LoadExamplePolicies(t *testing.T) {
	examples := []struct {
		name     string
		dir      string
		dataFile string
	}{
		{"fan-out-summarizer", "../../examples/fan-out-summarizer", "../../examples/fan-out-summarizer/data.json"},
		{"prompt-injection", "../../examples/prompt-injection", "../../examples/prompt-injection/data.json"},
	}
	for _, ex := range examples {
		t.Run(ex.name, func(t *testing.T) {
			modules, err := LoadRegoModules(ex.dir)
			if err != nil {
				t.Fatalf("LoadRegoModules: %v", err)
			}
			data, err := LoadDataFile(ex.dataFile)
			if err != nil {
				t.Fatalf("LoadDataFile: %v", err)
			}
			e, err := NewOPAEvaluator(context.Background(), modules, data)
			if err != nil {
				t.Fatalf("NewOPAEvaluator: %v", err)
			}

			r, err := e.EvaluateStep(context.Background(), stepInput("summarizer", "llm", ""))
			if err != nil {
				t.Fatalf("EvaluateStep: %v", err)
			}
			if !r.Permitted {
				t.Error("expected summarizer/llm to be permitted")
			}

			r, err = e.EvaluateStep(context.Background(), stepInput("evil", "http", "webassembly.org"))
			if err != nil {
				t.Fatalf("EvaluateStep: %v", err)
			}
			if r.Permitted {
				t.Error("expected unknown agent to be denied")
			}
		})
	}
}

func TestHostFnRegistry_Resolve(t *testing.T) {
	reg := NewHostFnRegistry()
	called := false
	reg.Register("test_fn", func(_ *Orchestrator) []extism.HostFunction {
		called = true
		return nil
	})

	reg.Resolve([]string{"test_fn"}, nil)
	if !called {
		t.Error("expected provider to be called")
	}

	called = false
	reg.Resolve([]string{"nonexistent"}, nil)
	if called {
		t.Error("expected provider NOT to be called for unknown name")
	}
}

func TestHostFnRegistry_Empty(t *testing.T) {
	reg := NewHostFnRegistry()
	fns := reg.Resolve([]string{"anything"}, nil)
	if len(fns) != 0 {
		t.Errorf("expected empty, got %d", len(fns))
	}
}

func TestEnrichParams_Domain(t *testing.T) {
	enrichments := []ParamEnrichment{
		{Source: "url", Target: "domain", Transform: "domain"},
	}
	params := map[string]string{"url": "https://example.com/path"}
	result := enrichParams(params, enrichments)

	if result["domain"] != "example.com" {
		t.Errorf("expected domain=example.com, got %q", result["domain"])
	}
	if result["url"] != "https://example.com/path" {
		t.Error("original param should be preserved")
	}
}

func TestEnrichParams_MissingSource(t *testing.T) {
	enrichments := []ParamEnrichment{
		{Source: "missing", Target: "domain", Transform: "domain"},
	}
	params := map[string]string{"url": "https://example.com"}
	result := enrichParams(params, enrichments)

	if _, ok := result["domain"]; ok {
		t.Error("enrichment should be skipped when source is missing")
	}
}

func TestEnrichParams_NoEnrichments(t *testing.T) {
	params := map[string]string{"key": "val"}
	result := enrichParams(params, nil)

	if result["key"] != "val" {
		t.Error("params should pass through unchanged")
	}
}

func TestGenericPlanBuilder(t *testing.T) {
	b := GenericPlanBuilder{}
	ctx := map[string]string{
		"steps": `[{"agent_type":"my-agent","params":{"key":"val"}},{"agent_type":"other","depends_on":["task-1-step-1"]}]`,
	}
	steps, err := b.BuildPlan("task-1", ctx, nil, nil)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(steps))
	}
	if steps[0].AgentType != "my-agent" {
		t.Errorf("step 0 agent_type=%q", steps[0].AgentType)
	}
	if steps[0].Params["key"] != "val" {
		t.Errorf("step 0 params=%v", steps[0].Params)
	}
	if len(steps[1].DependsOn) != 1 || steps[1].DependsOn[0] != "task-1-step-1" {
		t.Errorf("step 1 depends_on=%v", steps[1].DependsOn)
	}
}

func TestGenericPlanBuilder_MissingSteps(t *testing.T) {
	b := GenericPlanBuilder{}
	_, err := b.BuildPlan("task-1", map[string]string{}, nil, nil)
	if err == nil {
		t.Error("expected error when steps key is missing")
	}
}

func TestGenericPlanBuilder_EmptyArray(t *testing.T) {
	b := GenericPlanBuilder{}
	_, err := b.BuildPlan("task-1", map[string]string{"steps": "[]"}, nil, nil)
	if err == nil {
		t.Error("expected error for empty steps array")
	}
}
