package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
}

type llmMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type llmResponse struct {
	Content   string `json:"content"`
	ModelUsed string `json:"model_used"`
}

// llmHostFunctions returns the host functions to inject into the summarizer plugin.
func (o *Orchestrator) llmHostFunctions() []extism.HostFunction {
	fn := extism.NewHostFunctionWithStack(
		"llm_complete",
		func(ctx context.Context, p *extism.CurrentPlugin, stack []uint64) {
			inputBytes, err := p.ReadBytes(stack[0])
			if err != nil {
				o.logger.Error("llm_complete: read input", "err", err)
				stack[0] = 0
				return
			}

			var req llmRequest
			if err := json.Unmarshal(inputBytes, &req); err != nil {
				o.logger.Error("llm_complete: unmarshal", "err", err)
				stack[0] = 0
				return
			}

			var resp llmResponse
			if o.llmMode == "mock" {
				resp = mockLLM(req)
			} else {
				resp, err = realLLM(ctx, req, o.llmBaseURL, o.llmAPIKey, o.llmModel)
				if err != nil {
					o.logger.Error("llm_complete: upstream error", "err", err)
					stack[0] = 0
					return
				}
			}

			outputBytes, _ := json.Marshal(resp)
			offset, err := p.WriteBytes(outputBytes)
			if err != nil {
				o.logger.Error("llm_complete: write output", "err", err)
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

func mockLLM(req llmRequest) llmResponse {
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
	}
	body, _ := json.Marshal(openAIReq{
		Model: model, Messages: req.Messages,
		MaxTokens: req.MaxTokens, Temperature: req.Temperature,
	})

	url := strings.TrimRight(baseURL, "/") + "/v1/chat/completions"
	httpReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return llmResponse{}, fmt.Errorf("upstream request: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))

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
		return llmResponse{}, fmt.Errorf("unmarshal (status %d): %w", resp.StatusCode, err)
	}
	if len(apiResp.Choices) == 0 {
		return llmResponse{}, fmt.Errorf("no choices (status %d)", resp.StatusCode)
	}

	return llmResponse{
		Content:   apiResp.Choices[0].Message.Content,
		ModelUsed: apiResp.Model,
	}, nil
}
