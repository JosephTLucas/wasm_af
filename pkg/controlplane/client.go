package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
)

const (
	defaultRequestTimeout = 10 * time.Second
)

// Client sends wasmCloud control interface commands over NATS.
// It is safe for concurrent use.
type Client struct {
	nc      *nats.Conn
	lattice string // lattice prefix used in NATS subject routing
	timeout time.Duration
}

// NewClient creates a control plane client for the given lattice.
// lattice is the lattice name/prefix (typically "default" in local dev).
func NewClient(nc *nats.Conn, lattice string) *Client {
	return &Client{
		nc:      nc,
		lattice: lattice,
		timeout: defaultRequestTimeout,
	}
}

// WithTimeout returns a copy of the client with a custom request timeout.
func (c *Client) WithTimeout(d time.Duration) *Client {
	cp := *c
	cp.timeout = d
	return &cp
}

// subject builds a wasmbus control API NATS subject.
// Format: wasmbus.ctl.v1.<lattice>.<rest...>
func (c *Client) subject(parts ...string) string {
	base := "wasmbus.ctl.v1." + c.lattice
	for _, p := range parts {
		base += "." + p
	}
	return base
}

// request sends a JSON-encoded payload and decodes the ctl ack response.
func (c *Client) request(ctx context.Context, subject string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	msg, err := c.nc.RequestWithContext(reqCtx, subject, b)
	if err != nil {
		return fmt.Errorf("nats request to %s: %w", subject, err)
	}

	var ack ctlAck
	if err := json.Unmarshal(msg.Data, &ack); err != nil {
		return fmt.Errorf("unmarshal ack from %s: %w", subject, err)
	}
	if !ack.Accepted {
		return fmt.Errorf("control request rejected by host: %s", ack.Error)
	}
	return nil
}

// StartComponent scales a WASM component to one instance on the given host.
// componentRef is the OCI image reference (e.g. "localhost:5000/policy-engine:latest").
// componentID is the logical component ID used within the lattice.
func (c *Client) StartComponent(ctx context.Context, hostID, componentRef, componentID string) error {
	req := scaleComponentRequest{
		HostID:       hostID,
		ComponentRef: componentRef,
		ComponentID:  componentID,
		Count:        1,
	}
	return c.request(ctx, c.subject("component", "scale"), req)
}

// StopComponent scales a component to zero on the given host, effectively stopping it.
func (c *Client) StopComponent(ctx context.Context, hostID, componentID string) error {
	req := scaleComponentRequest{
		HostID:      hostID,
		ComponentID: componentID,
		Count:       0,
	}
	return c.request(ctx, c.subject("component", "scale"), req)
}

// PutLink creates or updates a runtime link between a source component and either
// a capability provider or another component (agent-to-agent direct comms).
func (c *Client) PutLink(ctx context.Context, link LinkDefinition) error {
	req := ctlLinkDefinition{
		SourceID:     link.SourceID,
		Target:       link.Target,
		Name:         link.linkName(),
		WitNamespace: link.WitNamespace,
		WitPackage:   link.WitPackage,
		Interfaces:   link.Interfaces,
		SourceConfig: link.SourceConfig,
		TargetConfig: link.TargetConfig,
	}
	return c.request(ctx, c.subject("link", "put"), req)
}

// DeleteLink removes a runtime link from the lattice.
// The source, link name, WIT namespace, and package are enough to identify the link.
func (c *Client) DeleteLink(ctx context.Context, sourceID, name, witNamespace, witPackage string) error {
	req := deleteLinkRequest{
		SourceID:     sourceID,
		Name:         name,
		WitNamespace: witNamespace,
		WitPackage:   witPackage,
	}
	return c.request(ctx, c.subject("link", "del"), req)
}

// GetHosts returns the set of currently active wasmCloud hosts in the lattice.
func (c *Client) GetHosts(ctx context.Context) ([]Host, error) {
	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	// Hosts respond on their own inbox subjects; we collect replies until timeout.
	sub, err := c.nc.SubscribeSync(nats.NewInbox())
	if err != nil {
		return nil, fmt.Errorf("subscribe inbox: %w", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	if err := c.nc.PublishRequest(c.subject("host", "get"), sub.Subject, nil); err != nil {
		return nil, fmt.Errorf("publish host.get: %w", err)
	}

	var hosts []Host
	for {
		msg, err := sub.NextMsgWithContext(reqCtx)
		if err != nil {
			break // timeout → done collecting
		}
		var h Host
		if err := json.Unmarshal(msg.Data, &h); err != nil {
			continue // skip malformed responses
		}
		hosts = append(hosts, h)
	}
	return hosts, nil
}

// GetInventory returns the running components and providers on a specific host.
func (c *Client) GetInventory(ctx context.Context, hostID string) (*HostInventory, error) {
	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	msg, err := c.nc.RequestWithContext(reqCtx, c.subject("host", hostID, "inv"), nil)
	if err != nil {
		return nil, fmt.Errorf("host inventory request: %w", err)
	}

	var inv HostInventory
	if err := json.Unmarshal(msg.Data, &inv); err != nil {
		return nil, fmt.Errorf("unmarshal inventory: %w", err)
	}
	return &inv, nil
}
