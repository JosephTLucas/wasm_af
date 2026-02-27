package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	extism "github.com/extism/go-sdk"
)

type sendEmailRequest struct {
	To      string `json:"to"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

type sendEmailResponse struct {
	Success   bool   `json:"success"`
	MessageID string `json:"message_id"`
	Error     string `json:"error"`
}

// NewEmailSendHostFnProvider returns a HostFnProvider that injects the
// send_email host function. In production this closure would capture SMTP
// credentials (host, port, username, password) — they never enter WASM memory.
// The mock implementation validates the recipient domain and logs delivery.
func NewEmailSendHostFnProvider(allowedDomains map[string]bool, logger *slog.Logger) HostFnProvider {
	return func(_ *Orchestrator) []extism.HostFunction {
		fn := extism.NewHostFunctionWithStack(
			"send_email",
			func(ctx context.Context, p *extism.CurrentPlugin, stack []uint64) {
				inputBytes, err := p.ReadBytes(stack[0])
				if err != nil {
					logger.Error("send_email: read input", "err", err)
					stack[0] = 0
					return
				}

				var req sendEmailRequest
				if err := json.Unmarshal(inputBytes, &req); err != nil {
					logger.Error("send_email: unmarshal", "err", err)
					stack[0] = 0
					return
				}

				resp := executeSendEmail(req, allowedDomains, logger)

				outputBytes, _ := json.Marshal(resp)
				offset, err := p.WriteBytes(outputBytes)
				if err != nil {
					logger.Error("send_email: write output", "err", err)
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

// executeSendEmail validates and mock-delivers an email. In production this
// would dial an SMTP server using credentials from the closure.
func executeSendEmail(req sendEmailRequest, allowedDomains map[string]bool, logger *slog.Logger) sendEmailResponse {
	if req.To == "" {
		return sendEmailResponse{Error: "recipient address is required"}
	}

	parts := strings.SplitN(req.To, "@", 2)
	if len(parts) != 2 || parts[1] == "" {
		return sendEmailResponse{Error: fmt.Sprintf("invalid recipient address: %s", req.To)}
	}
	domain := strings.ToLower(parts[1])

	if len(allowedDomains) > 0 && !allowedDomains[domain] {
		return sendEmailResponse{
			Error: fmt.Sprintf("recipient domain %q not in allowed list", domain),
		}
	}

	logger.Info("send_email: mock delivery",
		"to", req.To, "subject", req.Subject, "body_len", len(req.Body))

	return sendEmailResponse{
		Success:   true,
		MessageID: fmt.Sprintf("mock-msg-%s-%d", domain, 1),
	}
}
