package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/orchestrator/internal/sandboxcfg"
	"github.com/kuasar-sandbox/orchestrator/internal/types"
)

func TestCreateBackendInput(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want string
	}{
		{name: "omitted", body: `{}`},
		{name: "cloud hypervisor", body: `{"metadata":{"kuasar-sandbox.backend":"{\"name\":\"cloud-hypervisor\"}"}}`, want: "cloud-hypervisor"},
		{name: "firecracker", body: `{"metadata":{"kuasar-sandbox.backend":"{\"name\":\"firecracker\"}"}}`, want: "firecracker"},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			core := &checkpointCoreStub{create: func(_ context.Context, req CreateReq) (*types.Sandbox, error) {
				calls++
				if req.Backend != test.want {
					t.Fatalf("Core.Create Backend = %q, want %q", req.Backend, test.want)
				}
				if req.APIKey == "" {
					t.Fatal("Core.Create APIKey is empty")
				}
				if _, present := req.Metadata[sandboxcfg.NsBackend]; present {
					t.Fatal("public selector was not consumed before Core.Create")
				}
				return &types.Sandbox{ID: "created", Profile: types.ProfileBare}, nil
			}}
			handler, apiKey := newMigrationTestHandler(t, core)
			response := migrationRequest(t, handler, apiKey, http.MethodPost, "/sandboxes", strings.NewReader(test.body), nil)
			if response.Code != http.StatusCreated || calls != 1 {
				t.Fatalf("response = %d %q, calls = %d; want 201 and one core call", response.Code, response.Body.String(), calls)
			}
		})
	}
}

func TestCreateRejectsInvalidBackendBeforeCore(t *testing.T) {
	for _, body := range []string{
		`{"backend":""}`, `{"backend":null}`, `{"Backend":"firecracker"}`,
		`{"backend":"firecracker"}`, `{"backend":"cloud-hypervisor"}`,
		`{"backend":true}`, `{"backend":1}`, `{"backend":{}}`, `{"backend":[]}`,
		`{"backend":"qemu"}`, `{"backend":"Firecracker"}`, `{"backend":" firecracker"}`,
		`{"backend":"firecracker "}`, `{"backend":"default"}`,
		`{"backend":"firecracker","backend":"cloud-hypervisor"}`,
		`{"metadata":{"kuasar-sandbox.backend":null}}`,
		`{"metadata":{"kuasar-sandbox.backend":{"name":"firecracker"}}}`,
		`{"metadata":{"kuasar-sandbox.backend":true}}`,
		`{"metadata":{"kuasar-sandbox.backend":"null"}}`,
		`{"metadata":{"kuasar-sandbox.backend":"{\"name\":null}"}}`,
		`{"metadata":{"kuasar-sandbox.backend":"{\"name\":\"firecracker\",\"extra\":true}"}}`,
		`{"metadata":{"kuasar-sandbox.internal.backend":"firecracker"}}`,
	} {
		t.Run(body, func(t *testing.T) {
			core := &checkpointCoreStub{create: func(context.Context, CreateReq) (*types.Sandbox, error) {
				t.Fatal("invalid backend reached Core.Create")
				return nil, nil
			}}
			handler, apiKey := newMigrationTestHandler(t, core)
			response := migrationRequest(t, handler, apiKey, http.MethodPost, "/sandboxes", strings.NewReader(body), nil)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("response = %d %q, want 400", response.Code, response.Body.String())
			}
		})
	}
}

func TestCreateBackendJSONRoundTrip(t *testing.T) {
	for _, backend := range []string{"", "cloud-hypervisor", "firecracker"} {
		t.Run(backend, func(t *testing.T) {
			raw, err := json.Marshal(CreateReq{Backend: backend})
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			if _, present := fields["backend"]; present {
				t.Fatalf("internal backend leaked into public JSON: %s", raw)
			}
			var decoded CreateReq
			if err := json.Unmarshal(raw, &decoded); err != nil || decoded.Backend != "" {
				t.Fatalf("public JSON populated internal Backend = %q, error = %v", decoded.Backend, err)
			}
		})
	}
}
