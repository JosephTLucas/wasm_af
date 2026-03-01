package main

import (
	"context"
	"fmt"
	"time"

	extism "github.com/extism/go-sdk"
)

const validateTimeout = 5 * time.Second

// ValidateWASM loads a WASM binary from wasmBytes in a throwaway Extism
// plugin with zero capabilities and verifies that it exports an "execute"
// function. The plugin is destroyed before returning.
func ValidateWASM(ctx context.Context, wasmBytes []byte) error {
	if len(wasmBytes) == 0 {
		return fmt.Errorf("empty WASM binary")
	}

	manifest := extism.Manifest{
		Wasm: []extism.Wasm{
			extism.WasmData{Data: wasmBytes},
		},
		Memory: &extism.ManifestMemory{
			MaxPages: 16, // 1 MiB — just enough to load the module
		},
	}

	config := extism.PluginConfig{
		EnableWasi: true,
	}

	ctx, cancel := context.WithTimeout(ctx, validateTimeout)
	defer cancel()

	plugin, err := extism.NewPlugin(ctx, manifest, config, nil)
	if err != nil {
		return fmt.Errorf("WASM module failed to load: %w", err)
	}
	defer plugin.Close(ctx)

	if !plugin.FunctionExists("execute") {
		return fmt.Errorf("WASM module does not export an \"execute\" function")
	}

	return nil
}
