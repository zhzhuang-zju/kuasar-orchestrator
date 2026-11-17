package orch

import (
	"context"
	"fmt"

	"github.com/kuasar-sandbox/orchestrator/internal/api"
	"github.com/kuasar-sandbox/orchestrator/internal/types"
)

// Stored in the existing durable metadata JSON, never accepted from callers or
// Hooks. Rows predating backend selection are unambiguously Cloud Hypervisor.
const sandboxBackendKey = "kuasar-sandbox.internal.backend"

func sandboxBackend(sb *types.Sandbox) string {
	if sb != nil && sb.Metadata[sandboxBackendKey] != "" {
		return sb.Metadata[sandboxBackendKey]
	}
	return "cloud-hypervisor"
}

func (o *Orchestrator) firecracker() bool { return o.cfg.Sandbox.EffectiveBackend() == "firecracker" }

func (o *Orchestrator) rejectFirecracker(operation string) error {
	if o.firecracker() {
		return fmt.Errorf("%w: firecracker does not support %s; only disposable image cold create/delete and native exec are supported", api.ErrBadRequest, operation)
	}
	return nil
}

// Check before reconciliation touches a live runner, network, or resource
// placement. Changing node backend requires deleting all nonterminal sandboxes.
func (o *Orchestrator) validateBackendRecovery(ctx context.Context) error {
	for _, state := range []types.State{types.StateStarting, types.StateRunning, types.StatePausing, types.StatePaused} {
		if err := o.st.RangeByState(ctx, state, func(sb *types.Sandbox) error {
			if sandboxBackend(sb) != o.cfg.Sandbox.EffectiveBackend() {
				return fmt.Errorf("orch: cannot switch sandbox.backend to %s while sandbox %s uses %s; restore the previous backend and delete active sandboxes first", o.cfg.Sandbox.EffectiveBackend(), sb.ID, sandboxBackend(sb))
			}
			return nil
		}); err != nil {
			return err
		}
	}
	if o.firecracker() {
		if err := o.validateFirecrackerBuildRecovery(ctx); err != nil {
			return err
		}
		return o.st.RangeByState(ctx, types.StateDead, func(sb *types.Sandbox) error {
			if sb.ResetState == "pending" {
				return fmt.Errorf("orch: cannot switch to firecracker with pending sandbox reset %s", sb.ID)
			}
			return nil
		})
	}
	return nil
}

func (o *Orchestrator) validateFirecrackerBuildRecovery(ctx context.Context) error {
	if !o.firecracker() {
		return nil
	}
	for _, state := range []types.BuildState{types.BuildRegistered, types.BuildWaiting, types.BuildBuilding} {
		builds, err := o.st.BuildsByStatus(ctx, state)
		if err != nil {
			return err
		}
		if len(builds) != 0 {
			return fmt.Errorf("orch: cannot switch to firecracker with nonterminal build %s; complete or delete builds using cloud-hypervisor first", builds[0].BuildID)
		}
	}
	return nil
}
