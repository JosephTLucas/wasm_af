package main

import (
	"io"
	"log/slog"
	"testing"
)

var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestExecuteSendEmail_Success(t *testing.T) {
	domains := map[string]bool{"example.com": true}
	resp := executeSendEmail(sendEmailRequest{
		To:      "alice@example.com",
		Subject: "Hello",
		Body:    "World",
	}, domains, discardLogger)

	if !resp.Success {
		t.Errorf("expected success, got error: %s", resp.Error)
	}
	if resp.MessageID == "" {
		t.Error("expected non-empty message ID")
	}
}

func TestExecuteSendEmail_EmptyRecipient(t *testing.T) {
	resp := executeSendEmail(sendEmailRequest{To: ""}, nil, discardLogger)
	if resp.Success {
		t.Error("expected failure for empty recipient")
	}
	if resp.Error == "" {
		t.Error("expected error message")
	}
}

func TestExecuteSendEmail_InvalidAddress(t *testing.T) {
	cases := []string{"not-an-email", "missing-at-sign", "@", "user@"}
	for _, addr := range cases {
		t.Run(addr, func(t *testing.T) {
			resp := executeSendEmail(sendEmailRequest{To: addr}, nil, discardLogger)
			if resp.Success {
				t.Errorf("expected failure for invalid address %q", addr)
			}
		})
	}
}

func TestExecuteSendEmail_DomainNotAllowed(t *testing.T) {
	domains := map[string]bool{"example.com": true}
	resp := executeSendEmail(sendEmailRequest{
		To: "attacker@evil.com",
	}, domains, discardLogger)

	if resp.Success {
		t.Error("expected denial for non-allowed domain")
	}
	if resp.Error == "" {
		t.Error("expected error about disallowed domain")
	}
}

func TestExecuteSendEmail_EmptyAllowlist(t *testing.T) {
	resp := executeSendEmail(sendEmailRequest{
		To:      "anyone@anywhere.net",
		Subject: "Hi",
		Body:    "test",
	}, map[string]bool{}, discardLogger)

	if !resp.Success {
		t.Errorf("expected success with empty allowlist, got: %s", resp.Error)
	}
}

func TestExecuteSendEmail_CaseInsensitiveDomain(t *testing.T) {
	domains := map[string]bool{"example.com": true}
	resp := executeSendEmail(sendEmailRequest{
		To: "user@EXAMPLE.COM",
	}, domains, discardLogger)

	if !resp.Success {
		t.Errorf("expected case-insensitive domain match, got: %s", resp.Error)
	}
}
