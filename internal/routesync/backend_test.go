package routesync

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/kuasar-sandbox/orchestrator/internal/sandboxcfg"
)

func TestCommandBackendRoundTrip(t *testing.T) {
	for _, backend := range []string{"", "cloud-hypervisor", "firecracker"} {
		t.Run(backend, func(t *testing.T) {
			cmd := &Command{CmdID: "create", Kind: CmdCreate, Backend: backend}
			got := roundTrip(t, &Msg{Type: TypeCommand, Cmd: cmd})
			if got.Cmd.Backend != backend {
				t.Fatalf("backend = %q, want %q", got.Cmd.Backend, backend)
			}
			raw, err := json.Marshal(cmd)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			if _, present := fields["backend"]; present != (backend != "") {
				t.Fatalf("optional backend encoded incorrectly: %s", raw)
			}
		})
	}
}

func TestCommandBackendValidation(t *testing.T) {
	for _, kind := range []string{CmdCreate, CmdConnect, CmdExecSession, CmdDelete, CmdKeyPut, CmdKeyDrop, CmdBuildRegister} {
		for _, value := range []string{`""`, `null`, `"cloud-hypervisor"`, `"firecracker"`, `"qemu"`, `"Firecracker"`, `" firecracker"`, `true`, `1`, `{}`, `[]`} {
			t.Run(kind+"/"+value, func(t *testing.T) {
				raw := []byte(`{"type":"command","command":{"cmd_id":"c","kind":"` + kind + `","backend":` + value + `}}`)
				valid := kind == CmdCreate && (value == `""` || value == `null` || value == `"cloud-hypervisor"` || value == `"firecracker"`)
				var msg Msg
				err := json.Unmarshal(raw, &msg)
				if err == nil {
					err = ValidateMessage(&msg)
				}
				if (err == nil) != valid {
					t.Fatalf("ValidateMessage(%s) = %v, valid=%v", raw, err, valid)
				}
				var frame bytes.Buffer
				if err := binary.Write(&frame, binary.LittleEndian, uint32(len(raw))); err != nil {
					t.Fatal(err)
				}
				frame.Write(raw)
				if _, err := ReadMsg(&frame); (err == nil) != valid {
					t.Fatalf("ReadMsg(%s) = %v, valid=%v", raw, err, valid)
				}
			})
		}
		// Old commands with no backend field remain valid for every kind.
		if err := ValidateMessage(&Msg{Type: TypeCommand, Cmd: &Command{Kind: kind}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := ValidateMessage(&Msg{Type: TypeCommand, Cmd: &Command{Kind: CmdConnect, Backend: "firecracker"}}); err == nil {
		t.Fatal("programmatic non-create command accepted backend")
	}
}

func TestCommandRejectsUnnormalizedBackendMetadata(t *testing.T) {
	for _, key := range []string{sandboxcfg.NsBackend, sandboxcfg.InternalBackend} {
		for _, backend := range []string{"", "firecracker"} {
			msg := &Msg{Type: TypeCommand, Cmd: &Command{Kind: CmdCreate, Backend: backend, Config: map[string]string{key: `{"name":"firecracker"}`}}}
			if err := ValidateMessage(msg); err == nil {
				t.Fatalf("unnormalized selector accepted: %+v", msg.Cmd)
			}
			var frame bytes.Buffer
			if err := WriteMsg(&frame, msg); err == nil {
				t.Fatal("unnormalized selector written to node-link")
			}
		}
	}
}
