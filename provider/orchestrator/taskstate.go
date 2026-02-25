package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jolucas/wasm-af/pkg/taskstate"
)

// SubmitTaskRequest is the JSON body for POST /tasks.
type SubmitTaskRequest struct {
	Type    string            `json:"type"`
	Query   string            `json:"query"`
	Context map[string]string `json:"context,omitempty"`
}

// SubmitTaskResponse is returned on 202 Accepted.
type SubmitTaskResponse struct {
	TaskID string `json:"task_id"`
}

func (o *Orchestrator) handleSubmitTask(w http.ResponseWriter, r *http.Request) {
	var req SubmitTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Type == "" || req.Query == "" {
		http.Error(w, "type and query are required", http.StatusBadRequest)
		return
	}

	taskID := uuid.New().String()

	taskCtx := map[string]string{"query": req.Query}
	for k, v := range req.Context {
		taskCtx[k] = v
	}

	plan, err := o.buildPlan(req.Type, taskID, taskCtx)
	if err != nil {
		http.Error(w, fmt.Sprintf("unsupported task type: %s", req.Type), http.StatusUnprocessableEntity)
		return
	}

	state := &taskstate.TaskState{
		TaskID:    taskID,
		Status:    taskstate.StatusPending,
		Plan:      plan,
		Results:   make(map[string]string),
		Context:   taskCtx,
		CreatedAt: time.Now().UTC(),
	}

	ctx := r.Context()
	if err := o.store.Put(ctx, state); err != nil {
		o.logger.Error("failed to persist initial task state", "task_id", taskID, "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	_ = o.store.AppendAudit(ctx, &taskstate.AuditEvent{
		TaskID:    taskID,
		EventType: taskstate.EventTaskCreated,
		Message:   fmt.Sprintf("task created, type=%s query=%s", req.Type, req.Query),
	})

	go o.runTask(context.Background(), taskID)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(SubmitTaskResponse{TaskID: taskID})
}

func (o *Orchestrator) handleGetTask(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	if taskID == "" {
		http.Error(w, "missing task id", http.StatusBadRequest)
		return
	}

	state, err := o.store.Get(r.Context(), taskID)
	if err != nil {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(state)
}

func (o *Orchestrator) buildPlan(taskType, taskID string, taskCtx map[string]string) ([]taskstate.Step, error) {
	stepID := func(n int) string {
		return fmt.Sprintf("%s-step-%d", taskID, n)
	}

	switch taskType {
	case "research":
		return []taskstate.Step{
			{
				ID:        stepID(1),
				AgentType: "web-search",
				InputKey:  stepID(1) + ".input",
				OutputKey: stepID(1) + ".output",
				Status:    taskstate.StepPending,
			},
			{
				ID:        stepID(2),
				AgentType: "summarizer",
				InputKey:  stepID(2) + ".input",
				OutputKey: stepID(2) + ".output",
				Status:    taskstate.StepPending,
			},
		}, nil

	case "fan-out-summarizer":
		raw, ok := taskCtx["urls"]
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
				ID:           stepID(n),
				AgentType:    "url-fetch",
				InputKey:     stepID(n) + ".input",
				OutputKey:    stepID(n) + ".output",
				Status:       taskstate.StepPending,
				Group:        "fetch",
				AllowedHosts: domain,
				Params:       map[string]string{"url": u},
			}
			// Server-side enforcement: if an explicit allow list is configured,
			// deny any URL whose domain is absent — before a plugin is instantiated.
			// AllowedHosts comes from server config, never from user-submitted input.
			if !o.fetchDomainAllowed(domain) {
				step.Status = taskstate.StepDenied
				step.AllowedHosts = ""
				step.Error = fmt.Sprintf("domain %q is not in the server's URL fetch allow list", domain)
			}
			steps = append(steps, step)
		}

		sumN := len(urls) + 1
		steps = append(steps, taskstate.Step{
			ID:        stepID(sumN),
			AgentType: "summarizer",
			InputKey:  stepID(sumN) + ".input",
			OutputKey: stepID(sumN) + ".output",
			Status:    taskstate.StepPending,
		})

		return steps, nil

	case "isolation-test":
		// Creates a url-fetch step where AllowedHosts is set to one domain but
		// the URL targets a different domain. Used by the demo to prove per-instance
		// capability scoping: the plugin is instantiated but the cross-domain HTTP
		// call is rejected by the Extism runtime, not by the pre-flight allow list.
		//
		// The fetch_url domain must be in the server-side allow list — if it isn't,
		// the step would be denied at plan-build time, which demonstrates a different
		// (and less interesting) layer of enforcement.
		restrictedTo := taskCtx["restricted_to"]
		fetchURL := taskCtx["fetch_url"]
		if restrictedTo == "" || fetchURL == "" {
			return nil, fmt.Errorf("isolation-test requires 'restricted_to' and 'fetch_url' in context")
		}
		fetchDomain := extractDomain(fetchURL)
		step := taskstate.Step{
			ID:           stepID(1),
			AgentType:    "url-fetch",
			InputKey:     stepID(1) + ".input",
			OutputKey:    stepID(1) + ".output",
			Status:       taskstate.StepPending,
			AllowedHosts: restrictedTo,
			Params:       map[string]string{"url": fetchURL},
		}
		if !o.fetchDomainAllowed(fetchDomain) {
			step.Status = taskstate.StepDenied
			step.Error = fmt.Sprintf("domain %q is not in the server's URL fetch allow list", fetchDomain)
		}
		return []taskstate.Step{step}, nil

	default:
		return nil, fmt.Errorf("unknown task type %q", taskType)
	}
}

func extractDomain(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return rawURL
	}
	return u.Host
}
