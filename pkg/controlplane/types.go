// Package controlplane provides a client for the wasmCloud control interface API.
// It sends NATS request/reply messages on wasmbus.ctl.v1.* subjects to start and
// stop WASM components and manage their capability links at runtime.
//
// All linking in WASM_AF is done at runtime via PutLink; there is no build-time
// composition. TargetType distinguishes component→provider links from
// component→component direct wRPC links.
package controlplane

import "time"

// TargetType identifies whether a link target is a capability provider or another component.
type TargetType string

const (
	TargetProvider  TargetType = "provider"
	TargetComponent TargetType = "component"
)

// LinkDefinition describes a runtime link to be created between a source component
// and either a capability provider or a peer component.
type LinkDefinition struct {
	// SourceID is the wasmCloud component ID of the link source.
	SourceID string
	// Target is the component ID (TargetComponent) or provider key (TargetProvider).
	Target string
	// TargetType distinguishes provider links from direct component-to-component links.
	TargetType TargetType
	// Name is the link name. Defaults to "default" if empty.
	Name string
	// WitNamespace is the WIT package namespace (e.g. "wasm-af").
	WitNamespace string
	// WitPackage is the WIT package name (e.g. "agent", "llm", "policy").
	WitPackage string
	// Interfaces lists the WIT interface names covered by this link (e.g. ["handler"]).
	Interfaces []string
	// SourceConfig is arbitrary string config delivered to the source on link creation.
	SourceConfig map[string]string
	// TargetConfig is arbitrary string config delivered to the target on link creation.
	TargetConfig map[string]string
}

func (l *LinkDefinition) linkName() string {
	if l.Name == "" {
		return "default"
	}
	return l.Name
}

// Host represents a running wasmCloud host in the lattice.
type Host struct {
	ID        string            `json:"id"`
	Version   string            `json:"version,omitempty"`
	Uptime    uint64            `json:"uptime_seconds,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	LastSeen  time.Time         `json:"last_seen,omitempty"`
}

// ComponentInfo is a running component instance on a host.
type ComponentInfo struct {
	ID          string `json:"id"`
	ImageRef    string `json:"image_ref,omitempty"`
	Name        string `json:"name,omitempty"`
	MaxInstances uint32 `json:"max_instances,omitempty"`
}

// HostInventory holds the components and providers running on a single host.
type HostInventory struct {
	HostID     string          `json:"host_id"`
	Components []ComponentInfo `json:"components"`
}

// ctlAck is the standard wasmCloud control API acknowledgement envelope.
type ctlAck struct {
	Accepted bool   `json:"accepted"`
	Error    string `json:"error,omitempty"`
}

// scaleComponentRequest mirrors the wasmcloud control API ScaleComponent payload.
type scaleComponentRequest struct {
	HostID       string `json:"host_id"`
	ComponentRef string `json:"component_ref"`
	ComponentID  string `json:"component_id"`
	Count        uint32 `json:"count"`
}

// ctlLinkDefinition is the wire-format for the control API link put/del operations.
type ctlLinkDefinition struct {
	SourceID     string            `json:"source_id"`
	Target       string            `json:"target"`
	Name         string            `json:"name"`
	WitNamespace string            `json:"wit_namespace"`
	WitPackage   string            `json:"wit_package"`
	Interfaces   []string          `json:"interfaces"`
	SourceConfig map[string]string `json:"source_config,omitempty"`
	TargetConfig map[string]string `json:"target_config,omitempty"`
}

// deleteLinkRequest is the wire-format for the control API link del operation.
type deleteLinkRequest struct {
	SourceID     string `json:"source_id"`
	Name         string `json:"name"`
	WitNamespace string `json:"wit_namespace"`
	WitPackage   string `json:"wit_package"`
}
