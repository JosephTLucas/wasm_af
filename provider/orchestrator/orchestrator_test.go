package main

import (
	"fmt"
	"path/filepath"
	"testing"
)

func TestWasmPath_ValidName(t *testing.T) {
	o := &Orchestrator{wasmDir: "/tmp/wasm"}
	got, err := o.wasmPath("url_fetch")
	if err != nil {
		t.Fatalf("wasmPath returned error for valid name: %v", err)
	}

	want := filepath.Join("/tmp/wasm", "url_fetch.wasm")
	if got != want {
		t.Fatalf("wasmPath mismatch: got %q want %q", got, want)
	}
}

func TestWasmPath_RejectsTraversalAndInvalidNames(t *testing.T) {
	o := &Orchestrator{wasmDir: "/tmp/wasm"}
	cases := []string{
		"../evil",
		"..\\evil",
		"url/fetch",
		"url.fetch",
		"",
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := o.wasmPath(name); err == nil {
				t.Fatalf("expected error for invalid wasm name %q", name)
			}
		})
	}
}

func TestSanitizeAllowedPaths_Valid(t *testing.T) {
	in := map[string]string{
		"/tmp/workspace/../workspace": "/sandbox/./work",
	}
	got, err := sanitizeAllowedPaths(in)
	if err != nil {
		t.Fatalf("sanitizeAllowedPaths returned error: %v", err)
	}

	if got["/tmp/workspace"] != "/sandbox/work" {
		t.Fatalf("unexpected mapping: %#v", got)
	}
}

func TestSanitizeAllowedPaths_RejectsUnsafeValues(t *testing.T) {
	cases := []map[string]string{
		{"": "/sandbox"},
		{"/tmp/workspace": ""},
		{"../workspace": "/sandbox"},
		{"/tmp/workspace": "sandbox"},
		{"/tmp/workspace": "/"},
	}

	for i, tc := range cases {
		t.Run(fmt.Sprintf("case_%d", i), func(t *testing.T) {
			if _, err := sanitizeAllowedPaths(tc); err == nil {
				t.Fatalf("expected error for %#v", tc)
			}
		})
	}
}
