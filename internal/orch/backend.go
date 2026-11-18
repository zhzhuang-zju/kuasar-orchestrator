package orch

import (
	"context"
	"fmt"

	"github.com/kuasar-sandbox/orchestrator/internal/api"
	"github.com/kuasar-sandbox/orchestrator/internal/sandboxcfg"
	"github.com/kuasar-sandbox/orchestrator/internal/types"
)

// Persisted core-owned selection, distinct from the consumed public metadata
// selector and normalized Hook Backend. Legacy rows use Cloud Hypervisor.
const sandboxBackendKey = sandboxcfg.InternalBackend

func sandboxBackend(sb *types.Sandbox) string {
	if sb != nil && sb.Metadata[sandboxBackendKey] != "" {
		return sb.Metadata[sandboxBackendKey]
	}
	return "cloud-hypervisor"
}

func validateSandboxBackend(backend string) error {
	switch backend {
	case "cloud-hypervisor", "firecracker":
		return nil
	default:
		return fmt.Errorf("%w: unsupported sandbox backend %q", api.ErrBadRequest, backend)
	}
}

func (o *Orchestrator) effectiveSandboxBackend(requested string) (string, error) {
	if requested == "" {
		requested = o.cfg.Sandbox.EffectiveBackend()
	}
	return requested, validateSandboxBackend(requested)
}

// Call only after authorizing the target. Unknown persisted selections must
// never fall through to CH or inherit a newly configured node default.
func rejectSandboxFirecracker(sb *types.Sandbox, operation string) error {
	backend := sandboxBackend(sb)
	if err := validateSandboxBackend(backend); err != nil {
		return err
	}
	if backend == "firecracker" {
		return fmt.Errorf("%w: firecracker does not support %s; only disposable image cold create/delete and native exec are supported", api.ErrBadRequest, operation)
	}
	return nil
}

// Check before reconciliation touches a runner/network. Changing defaults is
// safe: each existing sandbox retains its own immutable selection.
func (o *Orchestrator) validateBackendRecovery(ctx context.Context) error {
	for _, state := range []types.State{types.StateStarting, types.StateRunning, types.StatePausing, types.StatePaused, types.StateDead, types.StateDeleting} {
		if err := o.st.RangeByState(ctx, state, func(sb *types.Sandbox) error {
			if err := validateSandboxBackend(sandboxBackend(sb)); err != nil {
				return fmt.Errorf("orch: sandbox %s: %w", sb.ID, err)
			}
			if sandboxBackend(sb) == "firecracker" {
				if state != types.StateDeleting && (state == types.StatePaused || state == types.StatePausing || !sb.ResumeSource.Empty()) {
					return fmt.Errorf("orch: firecracker sandbox %s has unsupported retained or paused state", sb.ID)
				}
				if sb.ResetState == "pending" {
					return fmt.Errorf("orch: firecracker has pending sandbox reset %s", sb.ID)
				}
				if state != types.StateDead && state != types.StateDeleting && o.cfg.ResourceListen != nil && o.cfg.ResourceListen.Enabled {
					return fmt.Errorf("orch: firecracker sandbox %s cannot use resource_listen until static accounting is supported", sb.ID)
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}
