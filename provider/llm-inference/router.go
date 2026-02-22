package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	nats "github.com/nats-io/nats.go"
)

// endpointConfig holds the per-link LLM endpoint configuration.
// One is registered for each agent component that links to this provider.
type endpointConfig struct {
	BaseURL      string // OpenAI-compatible API base, e.g. "https://api.openai.com"
	APIKey       string // bearer token; comes from link secrets, never stored in config
	DefaultModel string // fallback model if the agent doesn't specify one
}

// router manages per-link endpoint registrations and handles wRPC inference calls.
type router struct {
	mu     sync.RWMutex
	routes map[string]endpointConfig // sourceComponentID → config
	logger *slog.Logger
	client *http.Client
}

func newRouter(logger *slog.Logger) *router {
	return &router{
		routes: make(map[string]endpointConfig),
		logger: logger,
		client: &http.Client{Timeout: 120 * time.Second},
	}
}

func (r *router) registerRoute(sourceID string, cfg endpointConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routes[sourceID] = cfg
}

func (r *router) removeRoute(sourceID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.routes, sourceID)
}

func (r *router) routeFor(sourceID string) (endpointConfig, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cfg, ok := r.routes[sourceID]
	return cfg, ok
}

// subscribeWRPC subscribes to NATS subjects for wasm-af:llm/inference.complete.
// Subject format: wrpc.0.1.<providerKey>.wasm-af:llm/inference.complete
func (r *router) subscribeWRPC(nc *nats.Conn, providerKey string) error {
	subject := fmt.Sprintf("wrpc.0.1.%s.wasm-af:llm/inference.complete", providerKey)
	_, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		r.handleComplete(msg)
	})
	if err != nil {
		return fmt.Errorf("nats subscribe %s: %w", subject, err)
	}
	r.logger.Info("subscribed to wRPC inference.complete", "subject", subject)
	return nil
}

// wRPC message envelope wrapping the component ID of the caller.
type wrpcEnvelope struct {
	SourceID string          `json:"source_id"` // calling component's ID
	Payload  json.RawMessage `json:"payload"`
}

// completeRequest mirrors the WIT completion-request record.
type completeRequest struct {
	Model       string    `json:"model"`
	Messages    []message `json:"messages"`
	MaxTokens   uint32    `json:"max_tokens"`
	Temperature *float32  `json:"temperature,omitempty"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// completeResponse mirrors the WIT completion-response record on success,
// or an inference-error record on failure. Exactly one of Ok/Err will be set.
type completeResponse struct {
	Ok  *completionOk  `json:"ok,omitempty"`
	Err *inferenceError `json:"err,omitempty"`
}

type completionOk struct {
	Content   string     `json:"content"`
	ModelUsed string     `json:"model_used"`
	Usage     tokenUsage `json:"usage"`
}

type tokenUsage struct {
	PromptTokens     uint32 `json:"prompt_tokens"`
	CompletionTokens uint32 `json:"completion_tokens"`
}

type inferenceError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// handleComplete processes a single wasm-af:llm/inference.complete wRPC call.
func (r *router) handleComplete(msg *nats.Msg) {
	// Decode the wRPC envelope to get the calling component's ID.
	var envelope wrpcEnvelope
	if err := json.Unmarshal(msg.Data, &envelope); err != nil {
		r.replyError(msg, "invalid-request", fmt.Sprintf("bad wRPC envelope: %v", err))
		return
	}

	cfg, ok := r.routeFor(envelope.SourceID)
	if !ok {
		r.replyError(msg, "not-configured",
			fmt.Sprintf("no LLM route for component %s; is the link established?", envelope.SourceID))
		return
	}

	var req completeRequest
	if err := json.Unmarshal(envelope.Payload, &req); err != nil {
		r.replyError(msg, "invalid-request", fmt.Sprintf("bad completion-request: %v", err))
		return
	}

	if len(req.Messages) == 0 {
		r.replyError(msg, "invalid-request", "messages list is empty")
		return
	}

	model := req.Model
	if model == "" {
		model = cfg.DefaultModel
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	ok2, err := r.callOpenAI(ctx, cfg, model, req)
	if err != nil {
		r.logger.Error("OpenAI call failed",
			"source_id", envelope.SourceID, "model", model, "err", err)
		r.replyError(msg, errorCode(err), err.Error())
		return
	}

	r.replyOK(msg, ok2)
}

// openAIChatRequest is the OpenAI Chat Completions API request body.
type openAIChatRequest struct {
	Model       string    `json:"model"`
	Messages    []message `json:"messages"`
	MaxTokens   uint32    `json:"max_tokens"`
	Temperature *float32  `json:"temperature,omitempty"`
}

// openAIChatResponse is a minimal parse of the OpenAI Chat Completions response.
type openAIChatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     uint32 `json:"prompt_tokens"`
		CompletionTokens uint32 `json:"completion_tokens"`
	} `json:"usage"`
	Model string `json:"model"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error,omitempty"`
}

func (r *router) callOpenAI(
	ctx context.Context,
	cfg endpointConfig,
	model string,
	req completeRequest,
) (*completionOk, error) {
	body := openAIChatRequest{
		Model:       model,
		Messages:    req.Messages,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := strings.TrimRight(cfg.BaseURL, "/") + "/v1/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}

	resp, err := r.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("upstream request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20)) // 4 MB cap
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var apiResp openAIChatResponse
	if err := json.Unmarshal(raw, &apiResp); err != nil {
		return nil, fmt.Errorf("unmarshal response (status %d): %w", resp.StatusCode, err)
	}

	if apiResp.Error != nil {
		return nil, &upstreamError{
			msg:      apiResp.Error.Message,
			errType:  apiResp.Error.Type,
			httpCode: resp.StatusCode,
		}
	}

	if len(apiResp.Choices) == 0 {
		return nil, fmt.Errorf("upstream returned no choices (status %d)", resp.StatusCode)
	}

	return &completionOk{
		Content:   apiResp.Choices[0].Message.Content,
		ModelUsed: apiResp.Model,
		Usage: tokenUsage{
			PromptTokens:     apiResp.Usage.PromptTokens,
			CompletionTokens: apiResp.Usage.CompletionTokens,
		},
	}, nil
}

type upstreamError struct {
	msg      string
	errType  string
	httpCode int
}

func (e *upstreamError) Error() string {
	return fmt.Sprintf("upstream %s (HTTP %d): %s", e.errType, e.httpCode, e.msg)
}

func errorCode(err error) string {
	if ue, ok := err.(*upstreamError); ok {
		if ue.httpCode == http.StatusTooManyRequests {
			return "rate-limited"
		}
	}
	return "upstream-error"
}

func (r *router) replyOK(msg *nats.Msg, ok *completionOk) {
	resp := completeResponse{Ok: ok}
	b, _ := json.Marshal(resp)
	_ = msg.Respond(b)
}

func (r *router) replyError(msg *nats.Msg, code, message string) {
	resp := completeResponse{Err: &inferenceError{Code: code, Message: message}}
	b, _ := json.Marshal(resp)
	_ = msg.Respond(b)
}
