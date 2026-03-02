package main

import (
	"log/slog"

	extism "github.com/extism/go-sdk"
)

// HostFnProvider is a factory that returns Extism host functions to inject
// into a plugin. The provider captures its own configuration (API keys, etc.)
// in its closure — the Orchestrator is passed for access to shared resources
// like logging but providers should not rely on agent-specific Orchestrator
// fields.
type HostFnProvider func(o *Orchestrator) []extism.HostFunction

// HostFnRegistry maps host function names (as declared in agents.json)
// to their Go-side providers. runStep resolves the names dynamically
// instead of hardcoding if-chains.
type HostFnRegistry struct {
	providers map[string]HostFnProvider
	logger    *slog.Logger
}

func NewHostFnRegistry(logger *slog.Logger) *HostFnRegistry {
	return &HostFnRegistry{providers: make(map[string]HostFnProvider), logger: logger}
}

func (r *HostFnRegistry) Register(name string, p HostFnProvider) {
	r.providers[name] = p
}

// Has reports whether a provider is registered for the given name.
func (r *HostFnRegistry) Has(name string) bool {
	_, ok := r.providers[name]
	return ok
}

// Resolve collects all host functions for the given names. Unknown names
// are logged as warnings — misconfigured agent registries surface here
// rather than failing silently at plugin runtime.
func (r *HostFnRegistry) Resolve(names []string, o *Orchestrator) []extism.HostFunction {
	var fns []extism.HostFunction
	for _, name := range names {
		if p, ok := r.providers[name]; ok {
			fns = append(fns, p(o)...)
		} else {
			r.logger.Warn("host function not registered", "name", name)
		}
	}
	return fns
}
