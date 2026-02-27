package main

import (
	"context"
	"testing"
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

max_memory_pages := 64 if { input.step.agent_type == "url-fetch" }
max_memory_pages := 256 if { input.step.agent_type == "summarizer" }

allowed_hosts := [input.step.domain] if {
	input.step.agent_type == "url-fetch"
	input.step.domain
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
			"allowed_task_types": []string{"fan-out-summarizer", "research"},
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
	if r.DenyMessage == nil {
		t.Error("expected deny_message")
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
	if !r.Permitted {
		t.Error("expected allow for nil policy submit")
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
