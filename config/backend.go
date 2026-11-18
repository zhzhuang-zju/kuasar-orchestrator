package config

import (
	"fmt"
	"path/filepath"

	rtconfig "github.com/kuasar-sandbox/sandboxer/pkg/config"
)

// EffectiveBackend also supports programmatically constructed zero-value configs.
func (s SandboxConfig) EffectiveBackend() string {
	if s.Backend == "" {
		return "cloud-hypervisor"
	}
	return s.Backend
}

func (c *Conductor) validateBackend() error {
	switch c.Sandbox.EffectiveBackend() {
	case "cloud-hypervisor", "firecracker":
	default:
		return fmt.Errorf("config: sandbox.backend must be cloud-hypervisor or firecracker, got %q", c.Sandbox.Backend)
	}
	if c.Sandbox.FirecrackerBinary != "" && !filepath.IsAbs(c.Sandbox.FirecrackerBinary) {
		return fmt.Errorf("config: sandbox.firecracker_binary must be absolute")
	}
	// Backend resources are validated against the final per-Create selection,
	// not against this node default (which need not be used by any request).
	return nil
}

// ValidateBackendResources checks the final resolved values, not just the node
// defaults. In particular, omitted allocatable memory means 256MiB in the node
// schema, NOT the runtime's capacity-memory default. Never silently enlarge it.
func (s SandboxConfig) ValidateBackendResources(resources rtconfig.ResourcesConfig) error {
	switch s.EffectiveBackend() {
	case "cloud-hypervisor":
		return nil
	case "firecracker":
	default:
		return fmt.Errorf("unsupported sandbox backend %q", s.Backend)
	}
	if resources.Control.Controller != "" {
		return fmt.Errorf("firecracker: dynamic resource controller is unsupported")
	}
	if resources.DiffCOW.Discard == "manual" {
		return fmt.Errorf("firecracker: manual discard is unsupported")
	}
	cfg := &rtconfig.SandboxConfig{Resources: resources}
	capacity, err := cfg.CapacityMemoryBytes()
	if err != nil {
		return err
	}
	allocatable, err := cfg.AllocatableMemoryBytes()
	if err != nil {
		return err
	}
	startup, err := cfg.StartupBytes()
	if err != nil {
		return err
	}
	if capacity == 0 || capacity%(1<<20) != 0 || allocatable != capacity || startup != capacity {
		return fmt.Errorf("firecracker: capacity.memory must be positive whole MiB; explicitly set allocatable.memory equal to capacity.memory and omit startup.memory or set it equal (static memory only)")
	}
	if resources.Capacity.CPU < 1 || resources.Capacity.CPU > 32 {
		return fmt.Errorf("firecracker: capacity.cpu must be in [1,32]")
	}
	return nil
}
