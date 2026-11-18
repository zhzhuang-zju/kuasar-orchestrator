package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSandboxBackendConfig(t *testing.T) {
	for _, tc := range []struct{ name, backend, resources, extra, want string }{
		{name: "default"},
		{name: "explicit CH", backend: "cloud-hypervisor"},
		{name: "unknown", backend: "qemu", want: "sandbox.backend"},
		{name: "case sensitive", backend: "Firecracker", want: "sandbox.backend"},
		{name: "FC inherited 256MiB validated per request", backend: "firecracker"},
		{name: "FC static", backend: "firecracker", resources: "allocatable: {memory: 2GiB}"},
		{name: "FC equivalent bytes", backend: "firecracker", resources: "allocatable: {memory: 2048MiB}, startup: {memory: 2147483648}"},
		{name: "FC startup validated per request", backend: "firecracker", resources: "allocatable: {memory: 2GiB}, startup: {memory: 1GiB}"},
		{name: "FC fractional MiB validated per request", backend: "firecracker", resources: "capacity: {memory: '268435457'}, allocatable: {memory: '268435457'}"},
		{name: "FC CPU validated per request", backend: "firecracker", resources: "capacity: {cpu: 33}, allocatable: {memory: 2GiB}"},
		{name: "FC manual discard validated per request", backend: "firecracker", resources: "allocatable: {memory: 2GiB}, diff_cow: {discard: manual}"},
		{name: "FC dynamic default permits CH requests", backend: "firecracker", resources: "allocatable: {memory: 2GiB}", extra: "resource_listen: {enabled: true}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := "api: {domain: example.test}\nencryption_key: test-key\nsandbox:\n  boot: {kernel: /kernel, runtime: /runtime}\n"
			if tc.backend != "" {
				raw += "  backend: " + tc.backend + "\n"
			}
			if tc.resources != "" {
				raw += "  resources: {" + tc.resources + "}\n"
			}
			raw += tc.extra + "\n"
			cfg, err := LoadConductor(writeConfig(t, raw))
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("error = %v, want %q", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := tc.backend
			if want == "" {
				want = "cloud-hypervisor"
			}
			if cfg.Sandbox.Backend != want || cfg.Clone().Sandbox.Backend != want {
				t.Fatalf("backend = %q", cfg.Sandbox.Backend)
			}
			encoded, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			var restored Conductor
			if err := json.Unmarshal(encoded, &restored); err != nil {
				t.Fatal(err)
			}
			if restored.Sandbox.Backend != want {
				t.Fatalf("JSON backend = %q", restored.Sandbox.Backend)
			}
		})
	}
}

func TestFirecrackerBinaryConfig(t *testing.T) {
	for _, path := range []string{"firecracker", "/opt/bin/firecracker"} {
		cfg := &Conductor{Sandbox: SandboxConfig{FirecrackerBinary: path}}
		err := cfg.validateBackend()
		if (err != nil) != (path == "firecracker") {
			t.Fatalf("path %q: %v", path, err)
		}
	}
}
