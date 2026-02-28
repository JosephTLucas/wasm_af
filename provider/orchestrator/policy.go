package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/open-policy-agent/opa/ast"
	"github.com/open-policy-agent/opa/rego"
	"github.com/open-policy-agent/opa/storage"
	"github.com/open-policy-agent/opa/storage/inmem"
)

// OPAEvaluator wraps pre-compiled OPA queries for fast repeated evaluation.
// Two decision points are supported:
//   - stepQuery targets data.wasm_af.authz (step execution policy)
//   - submitQuery targets data.wasm_af.submit (task submission policy)
//
// Both share the same in-memory data store, which can be updated at
// runtime (e.g. from a NATS KV watcher) without recompiling the queries.
type OPAEvaluator struct {
	stepQuery   rego.PreparedEvalQuery
	submitQuery rego.PreparedEvalQuery
	store       storage.Store
}

// NewOPAEvaluator compiles the given Rego modules and prepares queries for
// step and submit evaluation. initialData populates the OPA data store
// (accessible as data.* in Rego). Pass nil for an empty store.
func NewOPAEvaluator(ctx context.Context, modules map[string]string, initialData map[string]any) (*OPAEvaluator, error) {
	if initialData == nil {
		initialData = map[string]any{}
	}
	store := inmem.NewFromObject(initialData)

	baseOpts := []func(*rego.Rego){
		rego.SetRegoVersion(ast.RegoV1),
		rego.Store(store),
	}
	for name, src := range modules {
		baseOpts = append(baseOpts, rego.Module(name, src))
	}

	stepOpts := append(append([]func(*rego.Rego){}, baseOpts...), rego.Query("x = data.wasm_af.authz"))
	stepPQ, err := rego.New(stepOpts...).PrepareForEval(ctx)
	if err != nil {
		return nil, fmt.Errorf("opa compile (authz): %w", err)
	}

	submitOpts := append(append([]func(*rego.Rego){}, baseOpts...), rego.Query("x = data.wasm_af.submit"))
	submitPQ, err := rego.New(submitOpts...).PrepareForEval(ctx)
	if err != nil {
		return nil, fmt.Errorf("opa compile (submit): %w", err)
	}

	return &OPAEvaluator{
		stepQuery:   stepPQ,
		submitQuery: submitPQ,
		store:       store,
	}, nil
}

// EvaluateStep runs the wasm_af.authz policy. Returns a structured
// PolicyResult that may include resource overrides (allowed_hosts,
// max_memory_pages, etc.) in addition to the allow/deny decision.
func (e *OPAEvaluator) EvaluateStep(ctx context.Context, input map[string]any) (*PolicyResult, error) {
	return evalQuery(ctx, e.stepQuery, input)
}

// EvaluateSubmit runs the wasm_af.submit policy. If the policy package
// is not defined in the loaded modules, the result defaults to allow —
// the submit gate is opt-in.
func (e *OPAEvaluator) EvaluateSubmit(ctx context.Context, input map[string]any) (*PolicyResult, error) {
	rs, err := e.submitQuery.Eval(ctx, rego.EvalInput(input))
	if err != nil {
		return nil, fmt.Errorf("opa eval (submit): %w", err)
	}

	if len(rs) == 0 || len(rs[0].Bindings) == 0 {
		return &PolicyResult{Permitted: true}, nil
	}

	submit, ok := rs[0].Bindings["x"].(map[string]any)
	if !ok {
		return &PolicyResult{Permitted: true}, nil
	}

	if _, defined := submit["allow"]; !defined {
		return &PolicyResult{Permitted: true}, nil
	}

	if allowed, _ := submit["allow"].(bool); allowed {
		return &PolicyResult{Permitted: true}, nil
	}

	result := &PolicyResult{Permitted: false}
	if msg, ok := submit["deny_message"].(string); ok {
		result.DenyMessage = &msg
	}
	if code, ok := submit["deny_code"].(string); ok {
		result.DenyCode = &code
	}
	return result, nil
}

// UpdateData writes a value into the OPA data store at the given path.
// Subsequent Eval calls see the new data without recompilation.
// Path uses OPA notation: "/config/allowed_domains".
func (e *OPAEvaluator) UpdateData(ctx context.Context, path string, value any) error {
	return storage.WriteOne(ctx, e.store, storage.AddOp, storage.MustParsePath(path), value)
}

func evalQuery(ctx context.Context, pq rego.PreparedEvalQuery, input map[string]any) (*PolicyResult, error) {
	rs, err := pq.Eval(ctx, rego.EvalInput(input))
	if err != nil {
		return nil, fmt.Errorf("opa eval: %w", err)
	}

	if len(rs) == 0 || len(rs[0].Bindings) == 0 {
		return &PolicyResult{
			Permitted:   false,
			DenyMessage: strPtr("policy evaluation returned no results"),
		}, nil
	}

	authz, ok := rs[0].Bindings["x"].(map[string]any)
	if !ok {
		return &PolicyResult{
			Permitted:   false,
			DenyMessage: strPtr("unexpected policy result type"),
		}, nil
	}

	if allowed, _ := authz["allow"].(bool); allowed {
		result := &PolicyResult{Permitted: true}
		readOverrides(authz, result)
		return result, nil
	}

	result := &PolicyResult{Permitted: false}
	if msg, ok := authz["deny_message"].(string); ok {
		result.DenyMessage = &msg
	}
	if code, ok := authz["deny_code"].(string); ok {
		result.DenyCode = &code
	}
	return result, nil
}

// readOverrides extracts optional structured decision fields from the
// Rego result into the PolicyResult. These let policy shape how a
// plugin is instantiated, not just whether it runs.
func readOverrides(authz map[string]any, r *PolicyResult) {
	if hosts, ok := authz["allowed_hosts"].([]any); ok {
		for _, h := range hosts {
			if s, ok := h.(string); ok {
				r.AllowedHosts = append(r.AllowedHosts, s)
			}
		}
	}
	if v, ok := toUint32(authz["max_memory_pages"]); ok {
		r.MaxMemPages = &v
	}
	if v, ok := toInt64(authz["max_http_bytes"]); ok {
		r.MaxHTTPBytes = &v
	}
	if v, ok := toInt(authz["timeout_sec"]); ok {
		r.TimeoutSec = &v
	}
	if fns, ok := authz["host_functions"].([]any); ok {
		for _, f := range fns {
			if s, ok := f.(string); ok {
				r.HostFunctions = append(r.HostFunctions, s)
			}
		}
	}
	if cfg, ok := authz["config"].(map[string]any); ok {
		r.Config = make(map[string]string, len(cfg))
		for k, v := range cfg {
			if s, ok := v.(string); ok {
				r.Config[k] = s
			}
		}
	}
	if paths, ok := authz["allowed_paths"].(map[string]any); ok {
		r.AllowedPaths = make(map[string]string, len(paths))
		for k, v := range paths {
			if s, ok := v.(string); ok {
				r.AllowedPaths[k] = s
			}
		}
	}
	if v, ok := authz["requires_approval"].(bool); ok {
		r.RequiresApproval = v
	}
	if v, ok := authz["approval_reason"].(string); ok {
		r.ApprovalReason = v
	}
}

// LoadRegoModules loads Rego source files from the given path.
// If path is a file, that single file is loaded.
// If path is a directory, all *.rego files in it are loaded (non-recursive).
// Test files (*_test.rego) are excluded.
func LoadRegoModules(path string) (map[string]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}

	modules := make(map[string]string)

	if !info.IsDir() {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		modules[filepath.Base(path)] = string(b)
		return modules, nil
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".rego") {
			continue
		}
		if strings.HasSuffix(e.Name(), "_test.rego") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(path, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		modules[e.Name()] = string(b)
	}
	if len(modules) == 0 {
		return nil, fmt.Errorf("no .rego files found in %s", path)
	}
	return modules, nil
}

// LoadDataFile reads a JSON file and returns it as a map suitable for
// passing as initialData to NewOPAEvaluator.
func LoadDataFile(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read data file %s: %w", path, err)
	}
	var data map[string]any
	if err := json.Unmarshal(b, &data); err != nil {
		return nil, fmt.Errorf("parse data file %s: %w", path, err)
	}
	return data, nil
}

func strPtr(s string) *string { return &s }

func toUint32(v any) (uint32, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, false
		}
		return uint32(i), true
	case float64:
		return uint32(n), true
	}
	return 0, false
}

func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, false
		}
		return i, true
	case float64:
		return int64(n), true
	}
	return 0, false
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, false
		}
		return int(i), true
	case float64:
		return int(n), true
	}
	return 0, false
}
