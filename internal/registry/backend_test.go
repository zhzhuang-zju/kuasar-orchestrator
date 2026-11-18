package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kuasar-sandbox/orchestrator/internal/routesync"
	"github.com/kuasar-sandbox/orchestrator/internal/sandboxcfg"
)

func TestServeReserveBackendPropagationAndIdempotency(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{}`, ""}, {`{"backend":""}`, ""}, {`{"backend":null}`, ""},
		{`{"backend":"cloud-hypervisor"}`, "cloud-hypervisor"},
		{`{"backend":"firecracker"}`, "firecracker"},
		// route-link is trusted internal wire; both typed selectors and raw
		// request config are normalized before reservation and node-link.
		{`{"config":{"kuasar-sandbox.backend":"{\"name\":\"cloud-hypervisor\"}"}}`, "cloud-hypervisor"},
		{`{"config":{"kuasar-sandbox.backend":"{\"name\":\"firecracker\"}"}}`, "firecracker"},
	} {
		t.Run(tc.body, func(t *testing.T) {
			ctx := context.Background()
			reg := testReg(t)
			reg.SetPlacer(placementWithToken("n1"))
			if err := reg.stores.PutNode(ctx, &NodeRecord{NodeID: "n1"}); err != nil {
				t.Fatal(err)
			}
			var creates atomic.Int32
			reg.addNode(&fakeConn{nodeID: "n1", onCmd: func(cmd *routesync.Command) {
				if _, present := cmd.Config[sandboxcfg.NsBackend]; present {
					t.Error("Registry forwarded an unnormalized metadata selector")
				}
				if cmd.Kind != routesync.CmdCreate {
					t.Errorf("unexpected command %s", cmd.Kind)
					return
				}
				creates.Add(1)
				var frame bytes.Buffer
				if err := routesync.WriteMsg(&frame, &routesync.Msg{Type: routesync.TypeCommand, Cmd: cmd}); err != nil {
					t.Error(err)
					return
				}
				decoded, err := routesync.ReadMsg(&frame)
				if err != nil || decoded.Cmd.Backend != tc.want {
					t.Errorf("node command=%+v error=%v, want backend %q", decoded, err, tc.want)
				}
				reserved, _, found, err := reg.stores.GetSandbox(ctx, "/g", "rk")
				if err != nil || !found || reserved.Backend != tc.want {
					t.Errorf("reserved=%+v found=%v err=%v", reserved, found, err)
				}
				go reg.ackCommand(&routesync.CmdAck{CmdID: cmd.CmdID, Status: routesync.AckAccepted})
				route := testE2BRoute(cmd.Cluster.StableID, routesync.StateRunning)
				go reg.applyRoute(ctx, "n1", &route)
			}})
			mux := http.NewServeMux()
			reg.ServeRouteLink(mux)
			reserve := func(body string) RouteResolve {
				t.Helper()
				req := httptest.NewRequest(http.MethodPost, RouteLinkReservePath+"?group=/g&route_key=rk&operation=create", strings.NewReader(body))
				req.Header.Set("X-API-KEY", testAPIKeyValue())
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, req)
				if w.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s", w.Code, w.Body)
				}
				var result ReserveResult
				if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				return result.Route
			}
			first := reserve(tc.body)
			// A retry cannot reconfigure an existing running logical sandbox.
			second := reserve(`{"backend":"firecracker"}`)
			if first.SandboxID != second.SandboxID || creates.Load() != 1 {
				t.Fatalf("idempotent create changed sandbox: first=%+v second=%+v creates=%d", first, second, creates.Load())
			}
			stored, _, found, err := reg.stores.GetSandbox(ctx, "/g", "rk")
			if err != nil || !found || stored.State != StateReady || stored.Backend != tc.want {
				t.Fatalf("ready=%+v found=%v err=%v", stored, found, err)
			}
		})
	}
}

func TestReserveBackendRejectsInvalidBeforePlacement(t *testing.T) {
	for _, operation := range []ReserveOperation{ReserveCreate, ReserveConnect, ReserveExecSession, ReserveData} {
		for _, value := range []string{`"qemu"`, `"Firecracker"`, `" firecracker"`, `"firecracker "`, `"default"`, `true`, `1`, `{}`, `[]`, `"firecracker","backend":"cloud-hypervisor"`, `"firecracker"`} {
			if operation == ReserveCreate && value == `"firecracker"` {
				continue
			}
			t.Run(string(operation)+"/"+value, func(t *testing.T) {
				reg := testReg(t)
				reg.SetPlacer(placementFunc(func(context.Context, PlaceRequest) (*Placement, error) {
					t.Error("invalid backend reached placement")
					return nil, ErrNoNode
				}))
				mux := http.NewServeMux()
				reg.ServeRouteLink(mux)
				req := httptest.NewRequest(http.MethodPost, RouteLinkReservePath+"?group=/g&route_key=rk&operation="+string(operation), strings.NewReader(`{"backend":`+value+`}`))
				req.Header.Set("X-API-KEY", testAPIKeyValue())
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, req)
				if w.Code != http.StatusBadRequest {
					t.Fatalf("status=%d body=%s", w.Code, w.Body)
				}
				if _, _, found, err := reg.stores.GetSandbox(context.Background(), "/g", "rk"); err != nil || found {
					t.Fatalf("invalid backend wrote route: found=%v err=%v", found, err)
				}
			})
		}
		// Typed callers must be validated independently of the HTTP decoder.
		for _, backend := range []string{"qemu", "firecracker"} {
			if operation == ReserveCreate && backend == "firecracker" {
				continue
			}
			reg := testReg(t)
			_, err := reg.ReserveSandbox(context.Background(), SandboxReserveRequest{Operation: operation, Group: "/g", RouteKey: "rk", Backend: backend})
			if !errors.Is(err, ErrReserveBadRequest) {
				t.Fatalf("operation=%s backend=%s: %v", operation, backend, err)
			}
		}
	}
}

func TestReserveBackendMetadataRejectsInvalidBeforePlacement(t *testing.T) {
	for _, config := range []map[string]string{
		{sandboxcfg.NsBackend: ""}, {sandboxcfg.NsBackend: "null"}, {sandboxcfg.NsBackend: "[]"},
		{sandboxcfg.NsBackend: `{}`}, {sandboxcfg.NsBackend: `{"name":null}`},
		{sandboxcfg.NsBackend: `{"name":1}`}, {sandboxcfg.NsBackend: `{"name":""}`},
		{sandboxcfg.NsBackend: `{"name":"qemu"}`}, {sandboxcfg.NsBackend: `{"Name":"firecracker"}`},
		{sandboxcfg.NsBackend: `{"name":"firecracker","extra":1}`},
		{sandboxcfg.NsBackend: `{"name":"firecracker","name":"cloud-hypervisor"}`},
		{sandboxcfg.NsBackend: `{"name":"firecracker"} {}`},
		{sandboxcfg.InternalBackend: ""}, {sandboxcfg.InternalBackend: "firecracker"},
	} {
		raw, err := json.Marshal(SandboxReserveReq{Config: config})
		if err != nil {
			t.Fatal(err)
		}
		t.Run(string(raw), func(t *testing.T) {
			reg := testReg(t)
			reg.SetPlacer(placementFunc(func(context.Context, PlaceRequest) (*Placement, error) {
				t.Fatal("invalid backend metadata reached placement")
				return nil, ErrNoNode
			}))
			mux := http.NewServeMux()
			reg.ServeRouteLink(mux)
			req := httptest.NewRequest(http.MethodPost, RouteLinkReservePath+"?group=/g&route_key=rk&operation=create", strings.NewReader(string(raw)))
			req.Header.Set("X-API-KEY", testAPIKeyValue())
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", w.Code, w.Body)
			}
			_, err := reg.ReserveSandbox(context.Background(), SandboxReserveRequest{Operation: ReserveCreate, Group: "/g", RouteKey: "rk", Config: config})
			if !errors.Is(err, ErrReserveBadRequest) {
				t.Fatalf("typed reserve accepted invalid metadata: %v", err)
			}
			if _, _, found, err := reg.stores.GetSandbox(context.Background(), "/g", "rk"); err != nil || found {
				t.Fatalf("invalid metadata wrote reservation: found=%v err=%v", found, err)
			}
		})
	}
	for _, operation := range []ReserveOperation{ReserveCreate, ReserveConnect, ReserveExecSession, ReserveData} {
		reg := testReg(t)
		// Even equal selectors are two authorities; non-create operations cannot
		// use the metadata carrier to evade their typed-field restriction.
		req := SandboxReserveRequest{Operation: operation, Group: "/g", RouteKey: "rk", Config: map[string]string{sandboxcfg.NsBackend: `{"name":"firecracker"}`}}
		if operation == ReserveCreate {
			req.Backend = "firecracker"
		}
		if _, err := reg.ReserveSandbox(context.Background(), req); !errors.Is(err, ErrReserveBadRequest) {
			t.Fatalf("operation=%s accepted duplicate/wrong-operation selector: %v", operation, err)
		}
	}
}

func TestReplacementAndReservedRetriesPreserveBackend(t *testing.T) {
	for _, scenario := range []struct {
		name         string
		state        SandboxState
		materialized bool
	}{
		{"ready", StateReady, true},
		{"reserved", StateReserved, true},
		{"unmaterialized", StateReserved, false},
	} {
		for _, backend := range []string{"", "cloud-hypervisor", "firecracker"} {
			t.Run(scenario.name+"/"+backend, func(t *testing.T) {
				ctx := context.Background()
				reg := testReg(t)
				original := testE2BSandboxRecord("/g", "rk", "sb-original", "old", scenario.state)
				if !scenario.materialized {
					original = testSandboxIdentityRecord("/g", "rk", "sb-original", "old", scenario.state)
				}
				original.Backend = backend
				if _, err := reg.stores.PutSandbox(ctx, original); err != nil {
					t.Fatal(err)
				}
				placements := 0
				reg.SetPlacer(placementFunc(func(context.Context, PlaceRequest) (*Placement, error) {
					placements++
					nodeID := "n1"
					if placements > 1 {
						nodeID = "n2"
					}
					return &Placement{NodeID: nodeID, TemplateRef: testTemplateRef, APISecretFingerprint: testAPIFingerprint}, nil
				}))
				for _, nodeID := range []string{"n1", "n2"} {
					if err := reg.stores.PutNode(ctx, &NodeRecord{NodeID: nodeID}); err != nil {
						t.Fatal(err)
					}
					reg.addNode(&fakeConn{nodeID: nodeID, onCmd: func(cmd *routesync.Command) {
						if cmd.Backend != backend {
							t.Errorf("retry backend=%q want durable %q", cmd.Backend, backend)
						}
						status := routesync.AckRejected
						if nodeID == "n2" {
							status = routesync.AckAccepted
						}
						reg.ackCommand(&routesync.CmdAck{CmdID: cmd.CmdID, Status: status})
					}})
				}
				var replacement *SandboxRecord
				if scenario.state == StateReady {
					replacement = original
				}
				override := "firecracker"
				if backend == override {
					override = "cloud-hypervisor"
				}
				if err := reg.placeAndCreate(ctx, "/g", "rk", nil, nil, replacement, true, false, createOptions{backend: override}); err != nil {
					t.Fatal(err)
				}
				reserved, _, found, err := reg.stores.GetSandbox(ctx, "/g", "rk")
				if err != nil || !found || reserved.Backend != backend || reserved.NodeID != "n2" || placements != 2 {
					t.Fatalf("reserved=%+v found=%v err=%v placements=%d", reserved, found, err, placements)
				}
				route := testRouteFromRecord(original, reserved.NodeSandboxID, routesync.StateRunning)
				if !scenario.materialized {
					route = testE2BRoute(original.SandboxID, routesync.StateRunning)
					route.SandboxID = reserved.NodeSandboxID
				}
				reg.applyRoute(ctx, "n2", &route)
				ready, _, found, err := reg.stores.GetSandbox(ctx, "/g", "rk")
				if err != nil || !found || ready.State != StateReady || ready.Backend != backend {
					t.Fatalf("ready=%+v found=%v err=%v", ready, found, err)
				}
			})
		}
	}
}

func TestCreateBackendCASRetryPreservesCommittedSelection(t *testing.T) {
	ctx := context.Background()
	reg := testReg(t)
	if err := reg.stores.PutNode(ctx, &NodeRecord{NodeID: "n1"}); err != nil {
		t.Fatal(err)
	}
	placements := 0
	reg.SetPlacer(placementFunc(func(context.Context, PlaceRequest) (*Placement, error) {
		placements++
		if placements == 1 {
			// A competing Registry commits the route while this placement is in flight.
			committed := testE2BSandboxRecord("/g", "rk", "sb-winner", "old", StateReserved)
			committed.Backend = "firecracker"
			if _, err := reg.stores.PutSandbox(ctx, committed); err != nil {
				t.Fatal(err)
			}
		}
		return &Placement{NodeID: "n1", TemplateRef: testTemplateRef, APISecretFingerprint: testAPIFingerprint}, nil
	}))
	commands := 0
	reg.addNode(&fakeConn{nodeID: "n1", onCmd: func(cmd *routesync.Command) {
		commands++
		if cmd.Backend != "firecracker" || cmd.Cluster.StableID != "sb-winner" {
			t.Errorf("CAS retry changed committed selection: %+v", cmd)
		}
		reg.ackCommand(&routesync.CmdAck{CmdID: cmd.CmdID, Status: routesync.AckAccepted})
	}})
	if err := reg.placeAndCreate(ctx, "/g", "rk", nil, nil, nil, true, false, createOptions{backend: "cloud-hypervisor"}); err != nil {
		t.Fatal(err)
	}
	stored, _, found, err := reg.stores.GetSandbox(ctx, "/g", "rk")
	if err != nil || !found || stored.Backend != "firecracker" || placements != 2 || commands != 1 {
		t.Fatalf("stored=%+v found=%v err=%v placements=%d commands=%d", stored, found, err, placements, commands)
	}
}
