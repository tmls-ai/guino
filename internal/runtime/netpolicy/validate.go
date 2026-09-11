package netpolicy

import (
	"fmt"
	"strings"

	"github.com/tmls-ai/guino/internal/runtime"
)

// denLabelPrefix is the reserved label namespace. Callers may not set it; the
// Docker runtime applies den.id/den.created authoritatively after the caller
// label loop, but the validator strips any den.* a caller tried to send so a
// caller can never influence ownership/spoof-resistance logic.
const denLabelPrefix = "den."

// ValidationError is returned (un-wrapped) for every caller-input violation so
// the HTTP and MCP handlers can errors.As it into a stable 400 body. The
// engine returns it before runtime.Create, so it is never fmt.Errorf-wrapped.
type ValidationError struct {
	Field string
	Msg   string
}

func (e *ValidationError) Error() string {
	if e.Field != "" {
		return fmt.Sprintf("%s: %s", e.Field, e.Msg)
	}
	return e.Msg
}

// Policy is the engine-held network policy. The zero value is valid: an empty
// DefaultMode is treated as internal (today's behavior, backward compatible),
// which is what the three test call sites pass.
type Policy struct {
	// DefaultMode is runtime.default_network_mode. "" ⇒ internal.
	DefaultMode runtime.NetworkMode
}

// EffectiveDefault returns the concrete global default ("" normalized to
// internal).
func (p Policy) EffectiveDefault() runtime.NetworkMode {
	if p.DefaultMode == "" {
		return runtime.NetworkModeInternal
	}
	return p.DefaultMode
}

// ResolveAndValidate validates the per-sandbox create request, resolves the
// effective network mode, normalizes/validates ports, strips den.* caller
// labels, and writes the resolved effective mode onto cfg.NetworkMode.
//
// On entry cfg.NetworkMode holds the raw per-sandbox requested value (only ""
// or "none" are accepted — a per-sandbox override may only INCREASE isolation).
// On success cfg.NetworkMode holds the concrete effective mode.
//
// Every violation returns a *ValidationError (never wrapped).
func (p Policy) ResolveAndValidate(cfg *runtime.SandboxConfig) error {
	// Per-sandbox ceiling: only "" (inherit) or "none" (more isolation).
	requested := cfg.NetworkMode
	switch requested {
	case "", runtime.NetworkModeNone:
		// ok
	default:
		return &ValidationError{
			Field: "network_mode",
			Msg:   `per-sandbox network_mode may only be omitted or "none" (it can only increase isolation)`,
		}
	}

	effective := p.EffectiveDefault()
	if requested == runtime.NetworkModeNone {
		effective = runtime.NetworkModeNone
	}

	// Normalize + validate ports. Protocol is case-insensitively {"", "tcp"}
	// and is rewritten to canonical lowercase "tcp" (nat.NewPort is
	// case-preserving, so an un-normalized "TCP" silently breaks publishing).
	for i := range cfg.Ports {
		pm := &cfg.Ports[i]
		switch strings.ToLower(pm.Protocol) {
		case "", "tcp":
			pm.Protocol = "tcp"
		default:
			return &ValidationError{
				Field: "ports.protocol",
				Msg:   fmt.Sprintf("unsupported protocol %q: only tcp is supported", pm.Protocol),
			}
		}
		if pm.SandboxPort < 1 || pm.SandboxPort > 65535 {
			return &ValidationError{
				Field: "ports.sandbox_port",
				Msg:   fmt.Sprintf("sandbox_port %d out of range (1-65535)", pm.SandboxPort),
			}
		}
		if pm.HostPort < 1 || pm.HostPort > 65535 {
			return &ValidationError{
				Field: "ports.host_port",
				Msg:   fmt.Sprintf("host_port %d out of range (1-65535)", pm.HostPort),
			}
		}
	}

	if effective == runtime.NetworkModeNone && len(cfg.Ports) > 0 {
		return &ValidationError{
			Field: "ports",
			Msg:   "network_mode=none cannot publish ports (the sandbox has no network)",
		}
	}

	// Strip any caller-supplied den.* labels; den.* is reserved.
	for k := range cfg.Labels {
		if strings.HasPrefix(k, denLabelPrefix) {
			delete(cfg.Labels, k)
		}
	}

	cfg.NetworkMode = effective
	return nil
}
