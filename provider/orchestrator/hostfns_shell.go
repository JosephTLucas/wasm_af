package main

// Shell host function — exec_command
//
// DESIGN NOTE — WASM-native alternatives (preferred over this host function):
//
// Most operations in the default command allowlist (ls, cat, head, tail, find,
// wc) are filesystem operations that CAN be implemented as WASM-native agents
// using WASI std::fs — exactly like the file-ops agent already does. This is
// the preferred approach because:
//
//   - WASI filesystem access is enforced by Wazero's AllowedPaths at the
//     runtime level. No Go code sits in the trust path.
//   - A compiled WASM binary's imports are immutable — a prompt injection
//     cannot add capabilities that weren't compiled in.
//   - The attack surface shrinks to the Wazero runtime, not /bin/sh.
//
// Roadmap for replacing this host function:
//   1. Extend file-ops with "list", "find", "head", "tail", "wc" operations.
//      All use WASI std::fs; no host function needed.
//   2. For date/uname: inject as plugin config or use WASI clocks.
//   3. Reserve exec_command ONLY for operations that genuinely require OS
//      process execution with no WASI equivalent. As of WASI preview-2, there
//      is no process-spawning capability (wasi:cli/command is proposals-only),
//      so this host function remains the escape hatch for that case.
//
// Until the above migration is complete, this host function is hardened with:
//   - exec.Command (no shell interpretation — metacharacters are literals)
//   - Binary allowlist (defense-in-depth behind OPA)
//   - Path argument confinement (defense-in-depth behind OPA)
//   - Restricted environment (no host secret leakage)
//   - Execution timeout

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	extism "github.com/extism/go-sdk"
)

type execRequest struct {
	Command string `json:"command"`
}

type execResponse struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

// shellMetachars are sequences that enable command chaining or redirection
// under /bin/sh. With exec.Command they are harmless literals, but rejecting
// them provides defense-in-depth against regressions.
var shellMetachars = []string{";", "|", "&", "`", "$(", ">", "<"}

// NewShellHostFnProvider returns a HostFnProvider that injects the exec_command
// host function. Commands are executed via exec.Command (NOT /bin/sh -c) so
// shell metacharacters are never interpreted.
//
// allowedCmds is a binary-name allowlist (defense-in-depth; OPA is primary).
// allowedPaths confines path-like arguments (defense-in-depth; OPA is primary).
// An empty list for either disables that host-side check.
func NewShellHostFnProvider(allowedCmds, allowedPaths []string, logger *slog.Logger) HostFnProvider {
	cmdSet := make(map[string]bool, len(allowedCmds))
	for _, c := range allowedCmds {
		if c = strings.TrimSpace(c); c != "" {
			cmdSet[c] = true
		}
	}

	pathBases := make([]string, 0, len(allowedPaths))
	for _, p := range allowedPaths {
		if p = strings.TrimSpace(p); p != "" {
			pathBases = append(pathBases, filepath.Clean(p))
		}
	}

	// Default working directory for spawned processes. Confines relative-path
	// operations to the first allowed base (or /tmp if none configured).
	workDir := "/tmp"
	if len(pathBases) > 0 {
		workDir = pathBases[0]
	}

	return func(_ *Orchestrator) []extism.HostFunction {
		fn := extism.NewHostFunctionWithStack(
			"exec_command",
			func(ctx context.Context, p *extism.CurrentPlugin, stack []uint64) {
				inputBytes, err := p.ReadBytes(stack[0])
				if err != nil {
					logger.Error("exec_command: read input", "err", err)
					stack[0] = 0
					return
				}

				var req execRequest
				if err := json.Unmarshal(inputBytes, &req); err != nil {
					logger.Error("exec_command: unmarshal", "err", err)
					stack[0] = 0
					return
				}

				if resp, ok := validateCommand(req.Command, cmdSet, pathBases); !ok {
					writeExecResponse(p, stack, resp, logger)
					return
				}

				args := strings.Fields(req.Command)
				resp := doExecCommand(ctx, args[0], args[1:], workDir, logger)
				writeExecResponse(p, stack, resp, logger)
			},
			[]extism.ValueType{extism.ValueTypePTR},
			[]extism.ValueType{extism.ValueTypePTR},
		)
		fn.SetNamespace("extism:host/user")
		return []extism.HostFunction{fn}
	}
}

// validateCommand applies host-side defense-in-depth checks. Returns a deny
// response and false if the command should be rejected.
func validateCommand(command string, cmdSet map[string]bool, pathBases []string) (execResponse, bool) {
	for _, mc := range shellMetachars {
		if strings.Contains(command, mc) {
			return execResponse{
				Stderr:   fmt.Sprintf("command contains disallowed character sequence %q", mc),
				ExitCode: -1,
			}, false
		}
	}

	args := strings.Fields(command)
	if len(args) == 0 {
		return execResponse{Stderr: "empty command", ExitCode: -1}, false
	}

	if len(cmdSet) > 0 && !cmdSet[args[0]] {
		return execResponse{
			Stderr:   "command binary not in allowed list",
			ExitCode: -1,
		}, false
	}

	if len(pathBases) > 0 {
		if err := validatePathArgs(args[1:], pathBases); err != nil {
			return execResponse{Stderr: err.Error(), ExitCode: -1}, false
		}
	}

	return execResponse{}, true
}

// validatePathArgs checks that any argument resembling a file path is confined
// to one of the allowed base paths. Relative paths containing traversal
// sequences are rejected outright.
func validatePathArgs(args []string, bases []string) error {
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		if strings.Contains(arg, "..") {
			return fmt.Errorf("path traversal (..) not allowed in argument %q", arg)
		}
		if !strings.HasPrefix(arg, "/") {
			continue
		}

		cleaned := filepath.Clean(arg)
		allowed := false
		for _, base := range bases {
			if cleaned == base || strings.HasPrefix(cleaned, base+"/") {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("path argument %q is not under any allowed base path", cleaned)
		}
	}
	return nil
}

func writeExecResponse(p *extism.CurrentPlugin, stack []uint64, resp execResponse, logger *slog.Logger) {
	outputBytes, _ := json.Marshal(resp)
	offset, err := p.WriteBytes(outputBytes)
	if err != nil {
		logger.Error("exec_command: write output", "err", err)
		stack[0] = 0
		return
	}
	stack[0] = offset
}

func doExecCommand(ctx context.Context, binary string, args []string, workDir string, logger *slog.Logger) execResponse {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Dir = workDir
	cmd.Env = []string{"PATH=/usr/bin:/bin"}

	exitCode := 0
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
			logger.Error("exec_command: run error", "err", err)
		}
	}

	return execResponse{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
	}
}
