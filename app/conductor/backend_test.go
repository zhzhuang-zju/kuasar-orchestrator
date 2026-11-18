package conductor_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kuasar-sandbox/orchestrator/app/conductor"
	conductorextension "github.com/kuasar-sandbox/orchestrator/app/conductor/extension"
)

type backendRoundTripHook struct {
	requested string
	selected  string
}

var _ conductor.SandboxHook = backendRoundTripHook{}

func (hook backendRoundTripHook) PrepareSandbox(_ context.Context, operation *conductor.SandboxOperation) error {
	raw, err := json.Marshal(operation.Create)
	if err != nil {
		return err
	}
	var request conductorextension.SandboxCreateRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return err
	}
	if request.Backend == hook.requested {
		request.Backend = hook.selected
	}
	operation.Create = &request
	return nil
}

func TestSandboxCreateHookBackendRoundTrip(t *testing.T) {
	for _, backend := range []string{"", "cloud-hypervisor", "firecracker"} {
		t.Run(backend, func(t *testing.T) {
			// The conductor type is an alias, so both public entry points expose
			// the same Backend field without a conversion or extra wrapper.
			original := conductor.SandboxCreateRequest{Backend: backend, TemplateID: "template"}
			candidate := original
			operation := conductor.SandboxOperation{
				Kind: conductor.SandboxOperationCreate, Create: &candidate,
			}
			hook := backendRoundTripHook{requested: backend, selected: "firecracker"}
			if err := hook.PrepareSandbox(context.Background(), &operation); err != nil {
				t.Fatal(err)
			}
			if operation.Create.Backend != "firecracker" || operation.Create.TemplateID != "template" {
				t.Fatalf("hook candidate = %+v", operation.Create)
			}
			if original.Backend != backend || candidate.Backend != backend {
				t.Fatal("hook changed the original string field")
			}
		})
	}
}

func TestSandboxCreateHookBackendJSON(t *testing.T) {
	for _, test := range []struct {
		body string
		want string
	}{
		{body: `{}`},
		{body: `{"backend":""}`},
		{body: `{"backend":null}`},
		{body: `{"backend":"cloud-hypervisor"}`, want: "cloud-hypervisor"},
		{body: `{"backend":"firecracker"}`, want: "firecracker"},
	} {
		t.Run(test.body, func(t *testing.T) {
			var request conductor.SandboxCreateRequest
			if err := json.Unmarshal([]byte(test.body), &request); err != nil {
				t.Fatal(err)
			}
			if request.Backend != test.want {
				t.Fatalf("Backend = %q, want %q", request.Backend, test.want)
			}
			raw, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			value, present := fields["backend"]
			if present != (test.want != "") || present && string(value) != `"`+test.want+`"` {
				t.Fatalf("backend wire field in %s, want %q (omitted if empty)", raw, test.want)
			}
			if _, present := fields["Backend"]; present {
				t.Fatalf("unexpected uppercase Backend wire field in %s", raw)
			}
		})
	}
	for _, body := range []string{`{"backend":true}`, `{"backend":1}`, `{"backend":{}}`, `{"backend":[]}`} {
		t.Run(body, func(t *testing.T) {
			var request conductor.SandboxCreateRequest
			if err := json.Unmarshal([]byte(body), &request); err == nil {
				t.Fatal("non-string backend decoded successfully")
			}
		})
	}
}

func TestSandboxCreateBackendPublicAlias(t *testing.T) {
	request := conductor.SandboxCreateRequest{Backend: "firecracker"}
	var public *conductorextension.SandboxCreateRequest = &request
	public.Backend = "cloud-hypervisor"
	if request.Backend != "cloud-hypervisor" {
		t.Fatal("public alias does not expose the same Backend field")
	}
}
