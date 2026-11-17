package main

import (
	"testing"

	"github.com/kuasar-sandbox/orchestrator/internal/types"
)

func TestValidateLaunchBackendArgs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		source types.ResumeSource
		valid  bool
	}{
		{name: "legacy CH", valid: true},
		{name: "explicit CH", args: []string{"--backend", "cloud-hypervisor", "--enable-checkpoint", "--retain-instance", "instance"}, valid: true},
		{name: "FC", args: []string{"--backend=firecracker", "--firecracker-binary", "/opt/bin/firecracker"}, valid: true},
		{name: "FC no checkpoint", args: []string{"--backend", "firecracker", "--enable-checkpoint=false"}, valid: true},
		{name: "missing", args: []string{"--backend"}},
		{name: "empty", args: []string{"--backend="}},
		{name: "flag value", args: []string{"--backend", "--config=x"}},
		{name: "unknown", args: []string{"--backend=qemu"}},
		{name: "whitespace", args: []string{"--backend=firecracker "}},
		{name: "duplicate", args: []string{"--backend=firecracker", "--backend", "cloud-hypervisor"}},
		{name: "same duplicate", args: []string{"--backend=firecracker", "--backend=firecracker"}},
		{name: "terminator", args: []string{"--", "--backend=firecracker"}},
		{name: "positional", args: []string{"ignored", "--backend=firecracker"}},
		{name: "single dash", args: []string{"-backend=firecracker"}},
		{name: "unknown flag", args: []string{"--backen=firecracker"}},
		{name: "bad boolean", args: []string{"--enable-checkpoint=perhaps"}},
		{name: "FC checkpoint", args: []string{"--backend=firecracker", "--enable-checkpoint"}},
		{name: "FC retention", args: []string{"--backend=firecracker", "--retain-instance=i"}},
		{name: "FC restore", args: []string{"--backend=firecracker"}, source: types.ResumeSource{Kind: types.ResumeSourceSnapshot, Ref: "snapshot"}},
		{name: "FC from", args: []string{"--backend=firecracker"}, source: types.ResumeSource{Kind: types.ResumeSourceSandbox, Ref: "sandbox"}},
		{name: "CH FC binary", args: []string{"--firecracker-binary=/bin/fc"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateLaunchBackendArgs(append([]string{"run"}, tc.args...), tc.source)
			if (err == nil) != tc.valid {
				t.Fatalf("error = %v, valid = %v", err, tc.valid)
			}
		})
	}
}
