package router

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/orchestrator/internal/clusterclient"
	"github.com/kuasar-sandbox/orchestrator/internal/registry"
	"github.com/kuasar-sandbox/orchestrator/internal/sandboxcfg"
	"github.com/kuasar-sandbox/orchestrator/internal/types"
)

func TestCreateBackendRouterPropagation(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{}`, ""},
		{`{"metadata":{"kuasar-sandbox.backend":"{\"name\":\"cloud-hypervisor\"}"}}`, "cloud-hypervisor"},
		{`{"metadata":{"kuasar-sandbox.backend":"{\"name\":\"firecracker\"}"}}`, "firecracker"},
	} {
		t.Run(tc.body, func(t *testing.T) {
			reserves := 0
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == registry.RouteLinkVerifyKeyPath {
					return textResponse(http.StatusOK, ""), nil
				}
				reserves++
				if r.URL.Path != registry.RouteLinkReservePath || r.URL.Query().Get("operation") != "create" {
					t.Fatalf("unexpected reserve: %s", r.URL)
				}
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				var wire registry.SandboxReserveReq
				if err := json.Unmarshal(raw, &wire); err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(raw, &fields); err != nil {
					t.Fatal(err)
				}
				_, present := fields["backend"]
				if wire.Backend != tc.want || present != (tc.want != "") {
					t.Fatalf("backend lost or default forced: %s, want %q", raw, tc.want)
				}
				if _, present := wire.Config["kuasar-sandbox.backend"]; present {
					t.Fatal("Router forwarded an unnormalized metadata selector")
				}
				body, err := json.Marshal(routerTestReserveResult(t, "s", "/g", "r", "node:1", types.ProfileBare))
				if err != nil {
					t.Fatal(err)
				}
				return textResponse(http.StatusOK, string(body)), nil
			})}
			rt := New("unused:80", "test.local", 0, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
			rt.routeRegistry = routeRegistryFunc(func(context.Context, string) ([]clusterclient.Endpoint, error) {
				return []clusterclient.Endpoint{{MemberID: "r1", BaseURL: "http://r1", Client: client}}, nil
			})
			req := httptest.NewRequest(http.MethodPost, "/sandboxes", strings.NewReader(tc.body))
			req.Host = "api.test.local"
			req.Header.Set(HeaderGroup, "/g")
			req.Header.Set(HeaderRouteKey, "r")
			req.Header.Set(HeaderAPIKey, "key")
			w := httptest.NewRecorder()
			rt.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusCreated || reserves != 1 {
				t.Fatalf("status=%d body=%s reserves=%d", w.Code, w.Body, reserves)
			}
		})
	}
}

func TestCreateBackendRouterRejectsInvalid(t *testing.T) {
	for _, value := range []string{`""`, `null`, `"firecracker"`, `"cloud-hypervisor"`, `"qemu"`, `"Firecracker"`, `" firecracker"`, `"firecracker "`, `"default"`, `true`, `1`, `{}`, `[]`, `"firecracker","backend":"cloud-hypervisor"`} {
		t.Run(value, func(t *testing.T) {
			rt := New("unused:80", "test.local", 0, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
			rt.routeRegistry = routeRegistryFunc(func(context.Context, string) ([]clusterclient.Endpoint, error) {
				t.Fatal("invalid backend reached registry")
				return nil, nil
			})
			req := httptest.NewRequest(http.MethodPost, "/sandboxes", strings.NewReader(`{"backend":`+value+`}`))
			req.Header.Set(HeaderGroup, "/g")
			w := httptest.NewRecorder()
			rt.handleCreate(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", w.Code, w.Body)
			}
		})
	}
}

func TestCreateBackendRouterRejectsMalformedMetadata(t *testing.T) {
	for _, value := range []any{
		nil, true, 1, map[string]string{"name": "firecracker"},
		"", "null", "[]", `{}`, `{"name":null}`, `{"name":true}`, `{"name":1}`,
		`{"name":""}`, `{"name":"qemu"}`, `{"Name":"firecracker"}`,
		`{"name":"firecracker","unknown":true}`,
		`{"name":"firecracker","name":"cloud-hypervisor"}`, `{"name":"firecracker"} {}`,
	} {
		raw, err := json.Marshal(map[string]any{"metadata": map[string]any{sandboxcfg.NsBackend: value}})
		if err != nil {
			t.Fatal(err)
		}
		t.Run(string(raw), func(t *testing.T) {
			rt := New("unused:80", "test.local", 0, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
			rt.routeRegistry = routeRegistryFunc(func(context.Context, string) ([]clusterclient.Endpoint, error) {
				t.Fatal("malformed backend metadata reached registry")
				return nil, nil
			})
			req := httptest.NewRequest(http.MethodPost, "/sandboxes", strings.NewReader(string(raw)))
			req.Header.Set(HeaderGroup, "/g")
			w := httptest.NewRecorder()
			rt.handleCreate(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", w.Code, w.Body)
			}
		})
	}
	for _, value := range []string{"", "firecracker"} {
		req := httptest.NewRequest(http.MethodPost, "/sandboxes", strings.NewReader(`{"metadata":{"kuasar-sandbox.internal.backend":"`+value+`"}}`))
		if _, err := createSandboxPoliciesWithBackend(httptest.NewRecorder(), req, nil, nil, nil); err == nil {
			t.Fatal("Router silently dropped protected internal backend metadata")
		}
	}
}
