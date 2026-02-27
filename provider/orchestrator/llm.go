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
	"time"

	extism "github.com/extism/go-sdk"
)

type llmRequest struct {
	Model       string       `json:"model"`
	Messages    []llmMessage `json:"messages"`
	MaxTokens   uint32       `json:"max_tokens"`
	Temperature *float32     `json:"temperature,omitempty"`
	TopP        *float32     `json:"top_p,omitempty"`
}

type llmMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type llmResponse struct {
	Content   string `json:"content"`
	ModelUsed string `json:"model_used"`
}

// LLMConfig holds all LLM-related configuration captured at startup.
type LLMConfig struct {
	Mode        string   // "mock", "real" (local Ollama), or "api" (remote OpenAI-compat)
	BaseURL     string
	APIKey      string
	Model       string
	Temperature *float32 // default when agent doesn't specify
	TopP        *float32 // default when agent doesn't specify
}

// NewLLMHostFnProvider returns a HostFnProvider that injects the llm_complete
// host function. All LLM configuration is captured in the closure — the
// Orchestrator struct has no LLM-specific fields.
func NewLLMHostFnProvider(cfg LLMConfig, logger *slog.Logger) HostFnProvider {
	return func(_ *Orchestrator) []extism.HostFunction {
		fn := extism.NewHostFunctionWithStack(
			"llm_complete",
			func(ctx context.Context, p *extism.CurrentPlugin, stack []uint64) {
				inputBytes, err := p.ReadBytes(stack[0])
				if err != nil {
					logger.Error("llm_complete: read input", "err", err)
					stack[0] = 0
					return
				}

				var req llmRequest
				if err := json.Unmarshal(inputBytes, &req); err != nil {
					logger.Error("llm_complete: unmarshal", "err", err)
					stack[0] = 0
					return
				}

				if req.Temperature == nil && cfg.Temperature != nil {
					req.Temperature = cfg.Temperature
				}
				if req.TopP == nil && cfg.TopP != nil {
					req.TopP = cfg.TopP
				}

				var resp llmResponse
				if cfg.Mode == "mock" {
					resp = mockLLM(req)
				} else {
					resp, err = realLLM(ctx, req, cfg.BaseURL, cfg.APIKey, cfg.Model)
					if err != nil {
						logger.Error("llm_complete: upstream error", "err", err)
						stack[0] = 0
						return
					}
				}

				outputBytes, _ := json.Marshal(resp)
				offset, err := p.WriteBytes(outputBytes)
				if err != nil {
					logger.Error("llm_complete: write output", "err", err)
					stack[0] = 0
					return
				}
				stack[0] = offset
			},
			[]extism.ValueType{extism.ValueTypePTR},
			[]extism.ValueType{extism.ValueTypePTR},
		)
		fn.SetNamespace("extism:host/user")
		return []extism.HostFunction{fn}
	}
}

func mockLLM(req llmRequest) llmResponse {
	for _, m := range req.Messages {
		if m.Role == "system" && strings.Contains(m.Content, "routing assistant") {
			return mockRouterLLM(req)
		}
	}
	return mockEchoLLM(req)
}

// mockRouterLLM returns valid router JSON so the skill pipeline is exercised.
func mockRouterLLM(req llmRequest) llmResponse {
	var userMsg string
	for _, m := range req.Messages {
		if m.Role == "user" {
			userMsg = m.Content
			break
		}
	}
	if idx := strings.Index(userMsg, "Current message: "); idx >= 0 {
		userMsg = userMsg[idx+len("Current message: "):]
	}

	route := mockRoute(strings.TrimSpace(userMsg))
	b, _ := json.Marshal(route)
	return llmResponse{Content: string(b), ModelUsed: "mock-router"}
}

type mockRouteResult struct {
	Skill  string            `json:"skill"`
	Params map[string]string `json:"params"`
}

func mockRoute(msg string) mockRouteResult {
	lower := strings.ToLower(msg)

	switch {
	case strings.HasPrefix(lower, "run "):
		return mockRouteResult{
			Skill:  "shell",
			Params: map[string]string{"command": strings.TrimSpace(msg[4:])},
		}

	case strings.Contains(lower, "list files"):
		path := "/tmp/wasmclaw"
		if idx := strings.Index(msg, "/"); idx >= 0 {
			path = strings.TrimSpace(msg[idx:])
		}
		return mockRouteResult{
			Skill:  "shell",
			Params: map[string]string{"command": "ls " + path},
		}

	case strings.Contains(lower, "send email") || strings.Contains(lower, "send an email"):
		to := "alice@example.com"
		subject := "Hello"
		body := msg
		if idx := strings.Index(lower, " to "); idx >= 0 {
			rest := msg[idx+4:]
			if sayIdx := strings.Index(strings.ToLower(rest), " saying "); sayIdx >= 0 {
				to = strings.TrimSpace(rest[:sayIdx])
				body = strings.TrimSpace(rest[sayIdx+7:])
				subject = body
				if len(subject) > 60 {
					subject = subject[:60]
				}
			}
		}
		return mockRouteResult{
			Skill:  "email-send",
			Params: map[string]string{"to": to, "subject": subject, "body": body},
		}

	case (strings.Contains(lower, "email") || strings.Contains(lower, "inbox")) &&
		(strings.Contains(lower, "check") || strings.Contains(lower, "read my")):
		return mockRouteResult{
			Skill:  "email-read",
			Params: map[string]string{"folder": "inbox"},
		}

	case strings.Contains(lower, "write") && strings.Contains(msg, " to /"):
		parts := strings.SplitN(msg, " to /", 2)
		raw := strings.TrimSpace(parts[0])
		idx := strings.Index(strings.ToLower(raw), "write ")
		content := raw
		if idx >= 0 {
			content = strings.TrimSpace(raw[idx+6:])
		}
		path := "/" + strings.TrimSpace(parts[1])
		return mockRouteResult{
			Skill:  "file-ops",
			Params: map[string]string{"op": "write", "path": path, "content": content},
		}

	case strings.Contains(lower, "read") && strings.Contains(msg, "/"):
		idx := strings.Index(msg, "/")
		path := strings.TrimSpace(msg[idx:])
		return mockRouteResult{
			Skill:  "file-ops",
			Params: map[string]string{"op": "read", "path": path},
		}

	case strings.Contains(lower, "fibonacci") || strings.Contains(lower, "calculate") || strings.Contains(lower, "compute"):
		code := "def fib(n):\n    a, b = 0, 1\n    for _ in range(n):\n        a, b = b, a + b\n    return a\nprint(fib(10))"
		return mockRouteResult{
			Skill:  "sandbox-exec",
			Params: map[string]string{"language": "python", "code": code},
		}

	case strings.HasPrefix(lower, "execute bash:"):
		code := strings.TrimSpace(msg[len("execute bash:"):])
		return mockRouteResult{
			Skill:  "sandbox-exec",
			Params: map[string]string{"language": "bash", "code": code},
		}

	default:
		return mockRouteResult{
			Skill:  "direct-answer",
			Params: map[string]string{},
		}
	}
}

func mockEchoLLM(req llmRequest) llmResponse {
	for _, m := range req.Messages {
		if m.Role == "user" {
			lo := strings.ToLower(m.Content)
			if strings.Contains(lo, "draft") && strings.Contains(lo, "reply") {
				return llmResponse{
					Content:   "[mock-llm] Draft reply: Thanks for your email! I've noted the details and will follow up accordingly.",
					ModelUsed: "mock-echo",
				}
			}
		}
	}
	var sb strings.Builder
	sb.WriteString("[mock-llm summary]\n\n")
	for _, m := range req.Messages {
		if m.Role == "user" {
			sb.WriteString(m.Content)
			sb.WriteString("\n")
		}
	}
	return llmResponse{
		Content:   sb.String(),
		ModelUsed: "mock-echo",
	}
}

const llmMaxRetries = 2

func realLLM(ctx context.Context, req llmRequest, baseURL, apiKey, defaultModel string) (llmResponse, error) {
	model := req.Model
	if model == "" {
		model = defaultModel
	}

	type openAIReq struct {
		Model       string       `json:"model"`
		Messages    []llmMessage `json:"messages"`
		MaxTokens   uint32       `json:"max_tokens"`
		Temperature *float32     `json:"temperature,omitempty"`
		TopP        *float32     `json:"top_p,omitempty"`
	}
	reqBody, _ := json.Marshal(openAIReq{
		Model: model, Messages: req.Messages,
		MaxTokens: req.MaxTokens, Temperature: req.Temperature,
		TopP: req.TopP,
	})

	base := strings.TrimRight(baseURL, "/")
	var endpoint string
	if strings.HasSuffix(base, "/v1") {
		endpoint = base + "/chat/completions"
	} else {
		endpoint = base + "/v1/chat/completions"
	}

	client := &http.Client{Timeout: 120 * time.Second}

	var lastErr error
	for attempt := 0; attempt <= llmMaxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return llmResponse{}, fmt.Errorf("context cancelled during retry: %w", ctx.Err())
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}

		httpReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
		httpReq.Header.Set("Content-Type", "application/json")
		if apiKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+apiKey)
		}

		resp, err := client.Do(httpReq)
		if err != nil {
			lastErr = fmt.Errorf("upstream request: %w", err)
			continue
		}

		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()

		if resp.StatusCode == 429 || resp.StatusCode == 502 || resp.StatusCode == 503 {
			lastErr = fmt.Errorf("transient HTTP %d: %s", resp.StatusCode, truncateBody(raw))
			continue
		}

		if resp.StatusCode != 200 {
			return llmResponse{}, fmt.Errorf("HTTP %d from LLM API: %s", resp.StatusCode, truncateBody(raw))
		}

		if len(raw) == 0 {
			lastErr = fmt.Errorf("empty response body (HTTP %d)", resp.StatusCode)
			continue
		}

		type openAIResp struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
			Model string `json:"model"`
		}

		var apiResp openAIResp
		if err := json.Unmarshal(raw, &apiResp); err != nil {
			return llmResponse{}, fmt.Errorf("unmarshal (HTTP %d, body %dB): %w", resp.StatusCode, len(raw), err)
		}
		if len(apiResp.Choices) == 0 {
			return llmResponse{}, fmt.Errorf("no choices in response (HTTP %d)", resp.StatusCode)
		}

		return llmResponse{
			Content:   apiResp.Choices[0].Message.Content,
			ModelUsed: apiResp.Model,
		}, nil
	}
	return llmResponse{}, fmt.Errorf("LLM request failed after %d attempts: %w", llmMaxRetries+1, lastErr)
}

func truncateBody(b []byte) string {
	if len(b) > 200 {
		return string(b[:200]) + "..."
	}
	return string(b)
}
