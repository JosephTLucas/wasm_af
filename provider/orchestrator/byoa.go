package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

const (
	maxWASMUploadBytes = 10 << 20 // 10 MiB
	untrustedCap       = "untrusted"
)

// RegisterAgentRequest is the JSON metadata accompanying a WASM upload.
// Capability and HostFunctions are forced to safe defaults for external
// agents — any values supplied by the caller are ignored.
type RegisterAgentRequest struct {
	Name       string `json:"name"`
	ContextKey string `json:"context_key"`
}

// RegisterAgentResponse is returned on successful agent registration.
type RegisterAgentResponse struct {
	Name       string `json:"name"`
	WasmName   string `json:"wasm_name"`
	Capability string `json:"capability"`
	External   bool   `json:"external"`
}

// handleRegisterAgent handles POST /agents.
// Expects a multipart form with:
//   - "meta" part: JSON RegisterAgentRequest
//   - "wasm" part: the compiled .wasm binary
func (o *Orchestrator) handleRegisterAgent(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxWASMUploadBytes+1<<20)

	if err := r.ParseMultipartForm(maxWASMUploadBytes); err != nil {
		http.Error(w, "request must be multipart/form-data within size limit", http.StatusBadRequest)
		return
	}

	metaJSON := r.FormValue("meta")
	if metaJSON == "" {
		http.Error(w, "missing \"meta\" form field", http.StatusBadRequest)
		return
	}
	var req RegisterAgentRequest
	if err := json.Unmarshal([]byte(metaJSON), &req); err != nil {
		http.Error(w, "invalid meta JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required in meta", http.StatusBadRequest)
		return
	}
	if !wasmNameRE.MatchString(req.Name) {
		http.Error(w, "name must match [A-Za-z0-9_-]+", http.StatusBadRequest)
		return
	}
	if req.ContextKey == "" {
		req.ContextKey = req.Name + "_result"
	}

	// Reject registration if the name collides with a platform (non-external) agent.
	if existing, err := o.registry.Get(req.Name); err == nil && !existing.External {
		http.Error(w, fmt.Sprintf("agent %q is a platform agent and cannot be overwritten", req.Name), http.StatusConflict)
		return
	}

	wasmFile, _, err := r.FormFile("wasm")
	if err != nil {
		http.Error(w, "missing \"wasm\" file part", http.StatusBadRequest)
		return
	}
	defer wasmFile.Close()

	wasmBytes, err := io.ReadAll(wasmFile)
	if err != nil {
		http.Error(w, "failed to read wasm file", http.StatusBadRequest)
		return
	}

	if err := ValidateWASM(r.Context(), wasmBytes); err != nil {
		http.Error(w, "WASM validation failed: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}

	extDir := filepath.Join(o.wasmDir, "external")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		o.logger.Error("failed to create external agent directory", "path", extDir, "err", err)
		http.Error(w, "internal error creating directory", http.StatusInternalServerError)
		return
	}
	destPath := filepath.Join(extDir, req.Name+".wasm")
	if err := os.WriteFile(destPath, wasmBytes, 0o644); err != nil {
		o.logger.Error("failed to write WASM file", "path", destPath, "err", err)
		http.Error(w, "internal error writing WASM file", http.StatusInternalServerError)
		return
	}

	meta := &AgentMeta{
		WasmName:      req.Name,
		Capability:    untrustedCap,
		ContextKey:    req.ContextKey,
		HostFunctions: []string{},
		External:      true,
	}
	if err := o.registry.Register(req.Name, meta); err != nil {
		_ = os.Remove(destPath)
		http.Error(w, "registry error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	o.logger.Info("external agent registered", "name", req.Name, "wasm_size", len(wasmBytes))
	o.persistExternalAgents(r.Context())

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(RegisterAgentResponse{
		Name:       req.Name,
		WasmName:   req.Name,
		Capability: untrustedCap,
		External:   true,
	})
}

// handleRemoveAgent handles DELETE /agents/{name}.
// Only external agents can be removed via this endpoint.
func (o *Orchestrator) handleRemoveAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "agent name is required", http.StatusBadRequest)
		return
	}

	meta, err := o.registry.Get(name)
	if err != nil {
		http.Error(w, fmt.Sprintf("agent %q not found", name), http.StatusNotFound)
		return
	}
	if !meta.External {
		http.Error(w, fmt.Sprintf("agent %q is a platform agent and cannot be removed via API", name), http.StatusForbidden)
		return
	}

	o.registry.Remove(name)

	wasmPath := filepath.Join(o.wasmDir, "external", meta.WasmName+".wasm")
	if err := os.Remove(wasmPath); err != nil && !os.IsNotExist(err) {
		o.logger.Error("failed to remove WASM file", "path", wasmPath, "err", err)
	}

	o.logger.Info("external agent removed", "name", name)
	o.persistExternalAgents(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// AgentListEntry is a single entry in the GET /agents response.
type AgentListEntry struct {
	Name          string   `json:"name"`
	WasmName      string   `json:"wasm_name"`
	Capability    string   `json:"capability"`
	HostFunctions []string `json:"host_functions"`
	External      bool     `json:"external"`
}

// handleListAgents handles GET /agents.
func (o *Orchestrator) handleListAgents(w http.ResponseWriter, _ *http.Request) {
	all := o.registry.List()
	entries := make([]AgentListEntry, 0, len(all))
	for name, meta := range all {
		entries = append(entries, AgentListEntry{
			Name:          name,
			WasmName:      meta.WasmName,
			Capability:    meta.Capability,
			HostFunctions: meta.HostFunctions,
			External:      meta.External,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(entries)
}

// ── NATS KV persistence for external agents ─────────────────────────────

const externalAgentsKVKey = "external-agents"

// persistExternalAgents writes the current set of external agents to
// NATS KV so they survive restarts and sync across replicas.
func (o *Orchestrator) persistExternalAgents(ctx context.Context) {
	if o.configKV == nil {
		return
	}
	ext := o.registry.ListExternal()
	b, err := json.Marshal(ext)
	if err != nil {
		o.logger.Error("failed to marshal external agents for KV", "err", err)
		return
	}
	if _, err := o.configKV.Put(ctx, externalAgentsKVKey, b); err != nil {
		o.logger.Error("failed to persist external agents to KV", "err", err)
	}
}

// seedExternalAgents parses a NATS KV value (JSON map of AgentMeta) and
// merges the entries into the registry. Only entries with External==true
// are accepted; anything else is silently skipped for safety.
func seedExternalAgents(registry *AgentRegistry, data []byte) (int, error) {
	var agents map[string]*AgentMeta
	if err := json.Unmarshal(data, &agents); err != nil {
		return 0, fmt.Errorf("parse external agents JSON: %w", err)
	}
	n := 0
	for name, meta := range agents {
		if !meta.External {
			continue
		}
		if err := registry.Register(name, meta); err != nil {
			return n, fmt.Errorf("register %q from KV: %w", name, err)
		}
		n++
	}
	return n, nil
}

// clearExternalAgents removes all external agents from the registry.
func clearExternalAgents(registry *AgentRegistry) {
	registry.ClearExternal()
}
