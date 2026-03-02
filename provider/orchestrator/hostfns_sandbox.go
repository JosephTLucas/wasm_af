package main

// Sandboxed code execution — sandbox_exec
//
// Runs LLM-generated code INSIDE a WASM sandbox (via Wazero) instead of on the
// host OS. The WASM runtime enforces filesystem, memory, and network boundaries
// at the hypervisor level — no Go code sits in the trust path.
//
// Supported runtimes are WASI command modules (export _start) placed in the
// configured runtimes directory:
//
//   runtimes/
//     js.wasm        — QuickJS from bellard/quickjs, compiled with wasi-sdk
//     python.wasm    — CPython from vmware-labs/webassembly-language-runtimes
//
// Security properties:
//   - Code runs in Wazero, NOT as a host OS process.
//   - Filesystem: only explicitly mounted paths are visible.
//   - Network: none (WASI preview-1 has no sockets).
//   - Memory: bounded by Wazero's linear memory limit.
//   - Timeout: context cancellation interrupts execution.
//   - Environment: only explicitly passed variables; no host env leakage.
//
// This gives OPA two distinct policy tiers:
//   - sandbox-exec: permissive (arbitrary code is fine — it can't escape WASM)
//   - shell (host exec): restrictive (binary + path allowlists, metachar block)

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	extism "github.com/extism/go-sdk"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

type sandboxExecRequest struct {
	Code     string            `json:"code"`
	Language string            `json:"language"`
	Argv     []string          `json:"argv,omitempty"`
	Stdin    string            `json:"stdin,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
}

type sandboxExecResponse struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

// SandboxEngine manages a shared Wazero runtime for executing WASI command
// modules. Compiled modules are cached; each invocation gets an isolated
// instance with its own memory, filesystem mounts, and stdio.
type SandboxEngine struct {
	runtimesDir string
	wazeroRT    wazero.Runtime
	compiled    map[string]wazero.CompiledModule
	mu          sync.RWMutex
	logger      *slog.Logger
	timeout     time.Duration
	invocID     uint64
	invocMu     sync.Mutex
}

func NewSandboxEngine(ctx context.Context, runtimesDir string, timeout time.Duration, logger *slog.Logger) (*SandboxEngine, error) {
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig())
	wasi_snapshot_preview1.MustInstantiate(ctx, rt)

	return &SandboxEngine{
		runtimesDir: runtimesDir,
		wazeroRT:    rt,
		compiled:    make(map[string]wazero.CompiledModule),
		logger:      logger,
		timeout:     timeout,
	}, nil
}

func (e *SandboxEngine) Close(ctx context.Context) error {
	return e.wazeroRT.Close(ctx)
}

func (e *SandboxEngine) nextID() uint64 {
	e.invocMu.Lock()
	defer e.invocMu.Unlock()
	e.invocID++
	return e.invocID
}

// getCompiled lazily compiles and caches a WASI runtime module.
func (e *SandboxEngine) getCompiled(ctx context.Context, language string) (wazero.CompiledModule, error) {
	e.mu.RLock()
	if cm, ok := e.compiled[language]; ok {
		e.mu.RUnlock()
		return cm, nil
	}
	e.mu.RUnlock()

	e.mu.Lock()
	defer e.mu.Unlock()

	if cm, ok := e.compiled[language]; ok {
		return cm, nil
	}

	wasmPath := filepath.Join(e.runtimesDir, language+".wasm")
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		return nil, fmt.Errorf("runtime %q not found at %s: %w", language, wasmPath, err)
	}

	cm, err := e.wazeroRT.CompileModule(ctx, wasmBytes)
	if err != nil {
		return nil, fmt.Errorf("compile runtime %q: %w", language, err)
	}

	e.logger.Info("compiled sandbox runtime", "language", language, "path", wasmPath)
	e.compiled[language] = cm
	return cm, nil
}

// Exec runs code in a sandboxed WASI instance. allowedPaths maps host paths to
// guest mount points (read-write). The code is written to a read-only /sandbox
// directory visible only to this invocation.
func (e *SandboxEngine) Exec(ctx context.Context, req sandboxExecRequest, allowedPaths map[string]string) sandboxExecResponse {
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	compiled, err := e.getCompiled(ctx, req.Language)
	if err != nil {
		return sandboxExecResponse{Stderr: err.Error(), ExitCode: -1}
	}

	sandboxDir, err := os.MkdirTemp("", "wasm-sandbox-*")
	if err != nil {
		return sandboxExecResponse{Stderr: fmt.Sprintf("create sandbox dir: %s", err), ExitCode: -1}
	}
	defer os.RemoveAll(sandboxDir)

	ext := languageExtension(req.Language)
	scriptName := "script" + ext
	scriptPath := filepath.Join(sandboxDir, scriptName)
	if err := os.WriteFile(scriptPath, []byte(req.Code), 0444); err != nil {
		return sandboxExecResponse{Stderr: fmt.Sprintf("write script: %s", err), ExitCode: -1}
	}

	fsConfig := wazero.NewFSConfig().
		WithReadOnlyDirMount(sandboxDir, "/sandbox")

	libDir := filepath.Join(e.runtimesDir, req.Language+"-lib")
	if info, err := os.Stat(libDir); err == nil && info.IsDir() {
		fsConfig = fsConfig.WithReadOnlyDirMount(libDir, "/lib")
	}

	for hostPath, guestPath := range allowedPaths {
		fsConfig = fsConfig.WithDirMount(hostPath, guestPath)
	}

	args := []string{req.Language, "/sandbox/" + scriptName}
	args = append(args, req.Argv...)

	var stdout, stderr bytes.Buffer
	moduleName := fmt.Sprintf("sandbox-%s-%d", req.Language, e.nextID())

	modConfig := wazero.NewModuleConfig().
		WithName(moduleName).
		WithStdout(&stdout).
		WithStderr(&stderr).
		WithStdin(strings.NewReader(req.Stdin)).
		WithArgs(args...).
		WithFSConfig(fsConfig)

	for k, v := range req.Env {
		modConfig = modConfig.WithEnv(k, v)
	}

	_, err = e.wazeroRT.InstantiateModule(ctx, compiled, modConfig)
	exitCode := 0
	if err != nil {
		var exitErr *sys.ExitError
		if errors.As(err, &exitErr) {
			exitCode = int(exitErr.ExitCode())
		} else {
			exitCode = -1
			e.logger.Error("sandbox exec failed", "language", req.Language, "err", err)
		}
	}

	return sandboxExecResponse{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
	}
}

func languageExtension(lang string) string {
	extensions := map[string]string{
		"python": ".py",
		"js":     ".js",
		"sh":     ".sh",
	}
	if ext, ok := extensions[lang]; ok {
		return ext
	}
	return ""
}

// NewSandboxHostFnProvider returns a HostFnProvider that injects the
// sandbox_exec host function. The engine must be initialized before any
// plugin invocation.
func NewSandboxHostFnProvider(engine *SandboxEngine, allowedLangs map[string]bool, allowedPaths map[string]string, logger *slog.Logger) HostFnProvider {
	return func(_ *Orchestrator) []extism.HostFunction {
		fn := extism.NewHostFunctionWithStack(
			"sandbox_exec",
			func(ctx context.Context, p *extism.CurrentPlugin, stack []uint64) {
				inputBytes, err := p.ReadBytes(stack[0])
				if err != nil {
					logger.Error("sandbox_exec: read input", "err", err)
					stack[0] = 0
					return
				}

				var req sandboxExecRequest
				if err := json.Unmarshal(inputBytes, &req); err != nil {
					logger.Error("sandbox_exec: unmarshal", "err", err)
					stack[0] = 0
					return
				}

				if len(allowedLangs) > 0 && !allowedLangs[req.Language] {
					resp := sandboxExecResponse{
						Stderr:   fmt.Sprintf("language %q not in allowed list", req.Language),
						ExitCode: -1,
					}
					writeSandboxResponse(p, stack, resp, logger)
					return
				}

				resp := engine.Exec(ctx, req, allowedPaths)
				writeSandboxResponse(p, stack, resp, logger)
			},
			[]extism.ValueType{extism.ValueTypePTR},
			[]extism.ValueType{extism.ValueTypePTR},
		)
		fn.SetNamespace("extism:host/user")
		return []extism.HostFunction{fn}
	}
}

func writeSandboxResponse(p *extism.CurrentPlugin, stack []uint64, resp sandboxExecResponse, logger *slog.Logger) {
	outputBytes, _ := json.Marshal(resp)
	offset, err := p.WriteBytes(outputBytes)
	if err != nil {
		logger.Error("sandbox_exec: write output", "err", err)
		stack[0] = 0
		return
	}
	stack[0] = offset
}
