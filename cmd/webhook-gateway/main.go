// Command webhook-gateway is a lightweight HTTP gateway that accepts chat
// messages, submits them as tasks to the orchestrator, polls for completion,
// and returns the response. It is the entry point for the wasmclaw demo.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const (
	defaultListenAddr      = ":8081"
	defaultOrchestratorURL = "http://localhost:8080"
	initialPollInterval    = 250 * time.Millisecond
	maxPollInterval        = 2 * time.Second
	maxPollDuration        = 30 * time.Second
)

var httpClient = &http.Client{
	Timeout: 30 * time.Second,
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	listenAddr := envOr("LISTEN_ADDR", defaultListenAddr)
	orchURL := envOr("ORCHESTRATOR_URL", defaultOrchestratorURL)

	gw := &gateway{logger: logger, orchURL: orchURL}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /message", gw.handleMessage)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{
		Addr:         listenAddr,
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		logger.Info("webhook-gateway listening", "addr", listenAddr, "orchestrator", orchURL)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("gateway error", "err", err)
			os.Exit(1)
		}
	}()

	<-sigCh
	logger.Info("shutdown requested")

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		logger.Error("shutdown error", "err", err)
	}
}

type gateway struct {
	logger  *slog.Logger
	orchURL string
}

// MessageRequest is the JSON body expected on POST /message.
type MessageRequest struct {
	Message string `json:"message"`
	User    string `json:"user"`
}

// MessageResponse is the JSON response from POST /message.
type MessageResponse struct {
	Response string `json:"response"`
	TaskID   string `json:"task_id"`
}

func (g *gateway) handleMessage(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10) // 64 KiB

	var req MessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Message == "" {
		http.Error(w, "message is required", http.StatusBadRequest)
		return
	}
	if req.User == "" {
		req.User = "anonymous"
	}

	g.logger.Info("message received", "user", req.User, "message_len", len(req.Message))

	taskID, err := g.submitTask(r.Context(), req)
	if err != nil {
		g.logger.Error("submit task", "err", err)
		http.Error(w, "failed to submit task", http.StatusBadGateway)
		return
	}

	g.logger.Info("task submitted", "task_id", taskID, "user", req.User)

	response, err := g.pollForResponse(r.Context(), taskID)
	if err != nil {
		g.logger.Error("poll task", "task_id", taskID, "err", err)
		http.Error(w, "task did not complete in time", http.StatusGatewayTimeout)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(MessageResponse{
		Response: response,
		TaskID:   taskID,
	})
}

// submitTask POSTs a chat task to the orchestrator and returns the task ID.
func (g *gateway) submitTask(ctx context.Context, req MessageRequest) (string, error) {
	body := map[string]any{
		"type":  "chat",
		"query": req.Message,
		"context": map[string]string{
			"user":    req.User,
			"message": req.Message,
		},
	}
	b, _ := json.Marshal(body)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, g.orchURL+"/tasks", bytes.NewReader(b))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("orchestrator request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return "", fmt.Errorf("orchestrator returned %d: %s", resp.StatusCode, raw)
	}

	var taskResp struct {
		TaskID string `json:"task_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&taskResp); err != nil {
		return "", fmt.Errorf("decode task response: %w", err)
	}
	return taskResp.TaskID, nil
}

// pollForResponse polls GET /tasks/{id} until the task completes and extracts
// the responder's output. Uses exponential backoff starting from 250ms up to 2s.
func (g *gateway) pollForResponse(ctx context.Context, taskID string) (string, error) {
	deadline := time.Now().Add(maxPollDuration)
	url := fmt.Sprintf("%s/tasks/%s", g.orchURL, taskID)
	interval := initialPollInterval

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(interval):
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return "", fmt.Errorf("build poll request: %w", err)
		}
		resp, err := httpClient.Do(httpReq)
		if err != nil {
			g.logger.Warn("poll request failed", "err", err)
			interval = backoff(interval)
			continue
		}

		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			interval = backoff(interval)
			continue
		}

		var state taskStateView
		if err := json.Unmarshal(raw, &state); err != nil {
			interval = backoff(interval)
			continue
		}

		switch state.Status {
		case "completed":
			return extractResponse(state), nil
		case "failed":
			return "", fmt.Errorf("task failed: %s", state.Error)
		}

		interval = backoff(interval)
	}

	return "", fmt.Errorf("timed out after %s", maxPollDuration)
}

func backoff(current time.Duration) time.Duration {
	next := current * 2
	if next > maxPollInterval {
		return maxPollInterval
	}
	return next
}

// taskStateView is a minimal view of the orchestrator's TaskState JSON.
type taskStateView struct {
	Status  string            `json:"status"`
	Error   string            `json:"error,omitempty"`
	Plan    []planStepView    `json:"plan"`
	Results map[string]string `json:"results"`
}

type planStepView struct {
	AgentType string `json:"agent_type"`
	OutputKey string `json:"output_key"`
}

// extractResponse finds the responder step output and extracts the response text.
func extractResponse(state taskStateView) string {
	for _, step := range state.Plan {
		if step.AgentType != "responder" {
			continue
		}
		payload, ok := state.Results[step.OutputKey]
		if !ok {
			continue
		}
		var out struct {
			Response string `json:"response"`
		}
		if err := json.Unmarshal([]byte(payload), &out); err == nil && out.Response != "" {
			return out.Response
		}
		return payload
	}
	return "(no response)"
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
