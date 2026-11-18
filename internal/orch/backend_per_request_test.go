package orch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	conductorextension "github.com/kuasar-sandbox/orchestrator/app/conductor/extension"
	"github.com/kuasar-sandbox/orchestrator/internal/api"
	"github.com/kuasar-sandbox/orchestrator/internal/config"
	"github.com/kuasar-sandbox/orchestrator/internal/nodectl"
	"github.com/kuasar-sandbox/orchestrator/internal/routesync"
	"github.com/kuasar-sandbox/orchestrator/internal/sandboxcfg"
	"github.com/kuasar-sandbox/orchestrator/internal/types"
)

const staticFirecrackerResource = `{"capacity":{"cpu":2,"memory":"2GiB"},"allocatable":{"memory":"2GiB"}}`

func TestBackendCreateHookSelectionRevalidatedAndImmutable(t *testing.T) {
	for _, test := range []struct {
		name, node, requested, final, resource, want, wantErr string
	}{
		{name: "hook selects FC", requested: "cloud-hypervisor", final: "firecracker", resource: staticFirecrackerResource, want: "firecracker"},
		{name: "hook selects CH on FC default", node: "firecracker", requested: "firecracker", final: "cloud-hypervisor", want: "cloud-hypervisor"},
		{name: "hook empty uses node default", node: "firecracker", requested: "cloud-hypervisor", final: "", resource: staticFirecrackerResource, want: "firecracker"},
		{name: "unknown request cannot reach hook", requested: "qemu", final: "cloud-hypervisor", wantErr: "unsupported sandbox backend"},
		{name: "unknown hook selection", final: "qemu", wantErr: "unsupported sandbox backend"},
		{name: "hook selected FC resources revalidated", final: "firecracker", wantErr: "allocatable.memory"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Sandbox.Backend = test.node
			lc, vs := &countingLauncher{}, &resourcePreflightVS{}
			o, ctx := newAsyncConnectTestOrchestrator(t, cfg, lc)
			o.vs = vs
			req := createRequestFixture(t, o, "5")
			req.Backend = test.requested
			var retained *conductorextension.SandboxCreateRequest
			o.SetExtensionHooks(sandboxHookFunc(func(_ context.Context, op *conductorextension.SandboxOperation) error {
				if op.Kind != conductorextension.SandboxOperationCreate {
					return nil
				}
				retained = op.Create
				expected := test.requested
				if expected == "" {
					expected = cfg.Sandbox.EffectiveBackend()
				}
				if op.Create.Backend != expected {
					t.Fatalf("hook backend = %q, want effective %q", op.Create.Backend, expected)
				}
				op.Create.Backend = test.final
				if test.resource != "" {
					op.Create.Metadata[sandboxcfg.NsResource] = test.resource
				}
				return nil
			}), nil)
			created, err := o.Create(ctx, req)
			if test.wantErr != "" {
				if !errors.Is(err, api.ErrBadRequest) || !strings.Contains(err.Error(), test.wantErr) || created != nil {
					t.Fatalf("Create = %+v, %v; want %q", created, err, test.wantErr)
				}
				if test.requested == "qemu" && retained != nil {
					t.Fatal("invalid request reached hook")
				}
				assertNoFreshLaunchSideEffects(t, o, ctx, vs, lc)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			retained.Backend = "qemu"
			retained.Metadata[sandboxBackendKey] = "qemu"
			created.Metadata[sandboxBackendKey] = "qemu"
			current := waitForSandbox(t, o, ctx, created.ID, func(sb *types.Sandbox) bool { return sb.State == types.StateRunning }, "post-hook backend launch")
			if sandboxBackend(current) != test.want {
				t.Fatalf("hook or return map changed durable backend: %+v", current.Metadata)
			}
			if _, err := o.launches.Wait(ctx, created.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := o.Kill(ctx, created.ID, req.APIKey); err != nil {
				t.Fatal(err)
			}
			waitForSandboxAbsent(t, o, ctx, created.ID, "post-hook backend deletion")
		})
	}
}

func TestBackendMetadataNormalizedBeforeHook(t *testing.T) {
	for _, inject := range []bool{false, true} {
		t.Run(fmt.Sprint("inject=", inject), func(t *testing.T) {
			lc, vs := &countingLauncher{}, &resourcePreflightVS{}
			o, ctx := newAsyncConnectTestOrchestrator(t, &config.Config{}, lc)
			o.vs = vs
			req := createRequestFixture(t, o, "8")
			req.Metadata[sandboxcfg.NsBackend] = `{"name":"firecracker"}`
			req.Metadata[sandboxcfg.NsResource] = staticFirecrackerResource
			called := false
			o.SetExtensionHooks(sandboxHookFunc(func(_ context.Context, op *conductorextension.SandboxOperation) error {
				if op.Kind != conductorextension.SandboxOperationCreate {
					return nil
				}
				called = true
				if op.Create.Backend != "firecracker" {
					t.Fatalf("Hook Backend = %q", op.Create.Backend)
				}
				if _, present := op.Create.Metadata[sandboxcfg.NsBackend]; present {
					t.Fatal("Hook received a second backend carrier")
				}
				if inject {
					op.Create.Metadata[sandboxcfg.NsBackend] = `{"name":"cloud-hypervisor"}`
				} else {
					op.Create.Backend = "cloud-hypervisor"
				}
				return nil
			}), nil)
			sb, err := o.Create(ctx, req)
			if !called {
				t.Fatal("Hook was not called")
			}
			if inject {
				if sb != nil || !errors.Is(err, api.ErrBadRequest) || !strings.Contains(err.Error(), "must be normalized") {
					t.Fatalf("Hook metadata injection = %+v, %v", sb, err)
				}
				assertNoFreshLaunchSideEffects(t, o, ctx, vs, lc)
				return
			}
			if err != nil || sandboxBackend(sb) != "cloud-hypervisor" {
				t.Fatalf("typed Hook override = %+v, %v", sb, err)
			}
			waitForSandbox(t, o, ctx, sb.ID, func(sb *types.Sandbox) bool { return sb.State == types.StateRunning }, "metadata Hook override")
			if _, err := o.launches.Wait(ctx, sb.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := o.Kill(ctx, sb.ID, req.APIKey); err != nil {
				t.Fatal(err)
			}
			waitForSandboxAbsent(t, o, ctx, sb.ID, "metadata Hook deletion")
		})
	}
}

func TestBackendCreateValidatesChosenResourcesNotDefault(t *testing.T) {
	for _, test := range []struct {
		name, node, requested, resource, wantErr string
		dynamic                                  bool
	}{
		{name: "CH on FC default keeps elastic defaults", node: "firecracker", requested: "cloud-hypervisor"},
		{name: "FC on CH default needs static memory", requested: "firecracker", wantErr: "allocatable.memory"},
		{name: "FC static request on CH default", requested: "firecracker", resource: staticFirecrackerResource},
		{name: "FC CPU range", requested: "firecracker", resource: `{"capacity":{"cpu":33,"memory":"2GiB"},"allocatable":{"memory":"2GiB"}}`, wantErr: "[1,32]"},
		{name: "FC whole MiB", requested: "firecracker", resource: `{"capacity":{"cpu":2,"memory":"268435457"},"allocatable":{"memory":"268435457"}}`, wantErr: "whole MiB"},
		{name: "FC startup equals capacity", requested: "firecracker", resource: `{"capacity":{"cpu":2,"memory":"2GiB"},"allocatable":{"memory":"2GiB"},"startup":{"memory":"1GiB"}}`, wantErr: "startup.memory"},
		{name: "FC manual discard", requested: "firecracker", resource: `{"capacity":{"cpu":2,"memory":"2GiB"},"allocatable":{"memory":"2GiB"},"diff_cow":{"discard":"manual"}}`, wantErr: "manual discard"},
		{name: "FC rejected with resource listen even when static", requested: "firecracker", resource: staticFirecrackerResource, dynamic: true, wantErr: "dynamic resource controller"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Sandbox.Backend = test.node
			if test.dynamic {
				cfg.ResourceListen = &config.ResourceListenConfig{Enabled: true}
			}
			lc, vs := &countingLauncher{}, &resourcePreflightVS{}
			o, ctx := newAsyncConnectTestOrchestrator(t, cfg, lc)
			o.vs = vs
			req := createRequestFixture(t, o, "6")
			req.Backend = test.requested
			if test.resource != "" {
				req.Metadata[sandboxcfg.NsResource] = test.resource
			}
			created, err := o.Create(ctx, req)
			if test.wantErr != "" {
				if !errors.Is(err, api.ErrBadRequest) || !strings.Contains(err.Error(), test.wantErr) || created != nil {
					t.Fatalf("Create = %+v, %v; want %q", created, err, test.wantErr)
				}
				assertNoFreshLaunchSideEffects(t, o, ctx, vs, lc)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			waitForSandbox(t, o, ctx, created.ID, func(sb *types.Sandbox) bool { return sb.State == types.StateRunning }, "backend resource launch")
			if _, err := o.launches.Wait(ctx, created.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := o.Kill(ctx, created.ID, req.APIKey); err != nil {
				t.Fatal(err)
			}
			waitForSandboxAbsent(t, o, ctx, created.ID, "backend resource deletion")
		})
	}
}

func TestBackendMixedConductorRetainsSelectionsAcrossDefaultChange(t *testing.T) {
	cfg := &config.Config{}
	o, ctx := newAsyncConnectTestOrchestrator(t, cfg, &countingLauncher{})
	var created []*types.Sandbox
	var apiKey string
	for _, backend := range []string{"cloud-hypervisor", "firecracker"} {
		req := createRequestFixture(t, o, "7")
		req.Backend = backend
		req.Metadata[sandboxcfg.NsResource] = staticFirecrackerResource
		sb, err := o.Create(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		apiKey = req.APIKey
		waitForSandbox(t, o, ctx, sb.ID, func(sb *types.Sandbox) bool { return sb.State == types.StateRunning }, "mixed backend running")
		if _, err := o.launches.Wait(ctx, sb.ID); err != nil {
			t.Fatal(err)
		}
		created = append(created, sb)
	}
	for _, nextDefault := range []string{"firecracker", "cloud-hypervisor"} {
		restartedConfig := cfg.Clone()
		restartedConfig.Sandbox.Backend = nextDefault
		restarted := &Orchestrator{cfg: restartedConfig, st: o.st, vs: stubVS{}, executables: o.executables}
		if err := restarted.validateBackendRecovery(ctx); err != nil {
			t.Fatalf("mixed recovery with default %s: %v", nextDefault, err)
		}
		for _, sb := range created {
			spec, _, found, err := restarted.LaunchSpecFor(ctx, "sandbox:"+sb.ID)
			if err != nil || !found || !hasArgPair(spec.Args, "--backend", sandboxBackend(sb)) {
				t.Fatalf("mixed durable launch = %+v, %t, %v", spec, found, err)
			}
		}
	}
	for _, sb := range created {
		if _, err := o.Kill(ctx, sb.ID, apiKey); err != nil {
			t.Fatal(err)
		}
		waitForSandboxAbsent(t, o, ctx, sb.ID, "mixed backend deletion")
	}
}

func TestBackendTTLUsesSandboxSelection(t *testing.T) {
	for _, backend := range []string{"cloud-hypervisor", "firecracker"} {
		t.Run(backend, func(t *testing.T) {
			cfg := checkpointOrchestratorConfig(t, config.CheckpointLocal)
			cfg.Sandbox.Backend = "firecracker"
			if backend == "firecracker" {
				cfg.Sandbox.Backend = "cloud-hypervisor"
			}
			o, sb, _, _, _, argsPath := newCheckpointPauseFixture(t, cfg, "")
			ctx := context.Background()
			if err := o.st.Delete(ctx, sb.ID); err != nil {
				t.Fatal(err)
			}
			sb.Metadata = map[string]string{sandboxBackendKey: backend}
			if err := o.st.Put(ctx, sb); err != nil {
				t.Fatal(err)
			}
			o.cache(sb)
			if err := o.pauseSandbox(ctx, sb); err != nil {
				t.Fatal(err)
			}
			if backend == "firecracker" {
				waitForSandboxAbsent(t, o, ctx, sb.ID, "FC TTL deletion")
				if args, err := os.ReadFile(argsPath); err == nil && len(args) > 0 {
					t.Fatalf("FC TTL attempted capture: %s", args)
				}
			} else {
				current, err := o.st.Get(ctx, sb.ID)
				if err != nil || current == nil || current.State != types.StatePaused || current.ResumeSource.Kind != types.ResumeSourceSnapshot {
					t.Fatalf("CH TTL on FC default = %+v, %v", current, err)
				}
			}
		})
	}
}

func TestBackendPressureExcludesFirecrackerAndUnknown(t *testing.T) {
	now := time.Now()
	rows := []*types.Sandbox{
		{ID: "fc", State: types.StateRunning, RunningSinceUnixNano: now.Add(-time.Hour).UnixNano(), Metadata: map[string]string{sandboxBackendKey: "firecracker"}},
		{ID: "unknown", State: types.StateRunning, RunningSinceUnixNano: now.Add(-time.Hour).UnixNano(), Metadata: map[string]string{sandboxBackendKey: "qemu"}},
		{ID: "legacy-ch", State: types.StateRunning, RunningSinceUnixNano: now.Add(-time.Minute).UnixNano()},
	}
	got := pressurePauseCandidate(rows, nodectl.PressureSnapshot{}, now, nil, time.Second, func(string) bool { return true })
	if got == nil || got.ID != "legacy-ch" {
		t.Fatalf("pressure candidate = %+v", got)
	}
}

func TestBackendRestrictionsPreserveAuthorization(t *testing.T) {
	cfg := &config.Config{}
	cfg.Sandbox.Backend = "firecracker"
	o := testOrchCfg(t, cfg)
	sb := backendSandboxFixture(t, cfg, types.StateRunning, "firecracker")
	ctx := context.Background()
	if err := o.st.Put(ctx, sb); err != nil {
		t.Fatal(err)
	}
	called := false
	o.SetExtensionHooks(sandboxHookFunc(func(context.Context, *conductorextension.SandboxOperation) error {
		called = true
		return nil
	}), nil)
	for _, id := range []string{sb.ID, "missing"} {
		for _, call := range []func() error{
			func() error {
				_, err := o.Pause(ctx, id, "wrong-key", sandboxcfg.CaptureRequest{Kind: types.CaptureSnapshot})
				return err
			},
			func() error { return o.Reset(ctx, id, "wrong-key") },
			func() error { _, err := o.ExportSandbox(ctx, "wrong-key", id, false, false, ""); return err },
			func() error { return o.AttachWorkspace(ctx, id, "wrong-key", "data", nil) },
			func() error { _, err := o.ExportWorkspace(ctx, id, "wrong-key", "data"); return err },
		} {
			if err := call(); !errors.Is(err, api.ErrNotFound) {
				t.Fatalf("unauthorized backend operation on %s = %v", id, err)
			}
		}
	}
	if called {
		t.Fatal("unauthorized backend operation reached hook")
	}
}

func TestBackendBuildRegistrationOnFirecrackerDefault(t *testing.T) {
	cfg := &config.Config{}
	cfg.Sandbox.Backend = "firecracker"
	o := testOrchCfg(t, cfg)
	key, _, _ := allowlistedBuildIdentity(t, o)
	build, err := o.RegisterBuild(context.Background(), key, api.RegisterSpec{
		Name: "ch-build-on-fc-default", Profile: types.ProfileBare, Resources: testBuildResources(),
	})
	if err != nil || build == nil {
		t.Fatalf("CH build registration on FC default = %+v, %v", build, err)
	}
	if err := o.validateBackendRecovery(context.Background()); err != nil {
		t.Fatalf("CH registered build blocked FC-default recovery: %v", err)
	}
}

func TestBackendMigrationRejectsNonCHSelection(t *testing.T) {
	for _, backend := range []string{"firecracker", "qemu", "Firecracker"} {
		_, err := migrationSandboxMetadata(map[string]string{sandboxBackendKey: backend})
		if !errors.Is(err, api.ErrBadRequest) {
			t.Fatalf("migration silently dropped backend %q: %v", backend, err)
		}
	}
}

func TestBackendBuildRejectsCreateOnlySelectors(t *testing.T) {
	for _, key := range []string{sandboxcfg.NsBackend, sandboxcfg.InternalBackend} {
		for _, fromHook := range []bool{false, true} {
			t.Run(key+fmt.Sprint("/hook=", fromHook), func(t *testing.T) {
				o := testOrchCfg(t, &config.Config{})
				apiKey, _, _ := allowlistedBuildIdentity(t, o)
				spec := api.RegisterSpec{Name: "backend-metadata-build", Profile: types.ProfileBare, Resources: testBuildResources()}
				if fromHook {
					o.SetExtensionHooks(nil, buildHookFunc(func(_ context.Context, op *conductorextension.BuildOperation) error {
						if op.Register.Metadata == nil {
							op.Register.Metadata = map[string]string{}
						}
						op.Register.Metadata[key] = `{"name":"firecracker"}`
						return nil
					}))
				} else {
					spec.Metadata = map[string]string{key: `{"name":"firecracker"}`}
				}
				build, err := o.RegisterBuild(context.Background(), apiKey, spec)
				if build != nil || !errors.Is(err, api.ErrBadRequest) || !strings.Contains(err.Error(), "not valid for template builds") {
					t.Fatalf("build accepted Create-only selector: %+v, %v", build, err)
				}
			})
		}
	}
}

func TestBackendClusterHookCannotReintroduceMetadataSelector(t *testing.T) {
	for _, key := range []string{sandboxcfg.NsBackend, sandboxcfg.InternalBackend} {
		lc, vs := &countingLauncher{}, &resourcePreflightVS{}
		o, ctx := newAsyncConnectTestOrchestrator(t, &config.Config{}, lc)
		o.vs = vs
		_, _, fingerprint := allowlistedBuildIdentity(t, o)
		cmd := clusterCreateCommand(fingerprint, "backend-hook-injection")
		cmd.Backend = "cloud-hypervisor"
		o.SetExtensionHooks(sandboxHookFunc(func(_ context.Context, op *conductorextension.SandboxOperation) error {
			op.Create.Metadata = map[string]string{key: `{"name":"firecracker"}`}
			return nil
		}), nil)
		ack := o.HandleCommand(ctx, cmd)
		if ack.Status != routesync.AckRejected || ack.HTTPStatus != 400 {
			t.Fatalf("cluster Hook reintroduced %s: %+v", key, ack)
		}
		assertNoFreshLaunchSideEffects(t, o, ctx, vs, lc)
	}
}

func TestBackendUnknownLaunchAndFirecrackerResumeFailClosed(t *testing.T) {
	cfg := &config.Config{}
	o := testOrchCfg(t, cfg)
	ctx := context.Background()
	sb := backendSandboxFixture(t, cfg, types.StatePaused, "qemu")
	if err := o.st.Put(ctx, sb); err != nil {
		t.Fatal(err)
	}
	if spec, _, _, err := o.LaunchSpecFor(ctx, "sandbox:"+sb.ID); spec != nil || !errors.Is(err, api.ErrBadRequest) {
		t.Fatalf("unknown backend launch spec = %+v, %v", spec, err)
	}
	for _, backend := range []string{"qemu", "firecracker"} {
		if err := o.st.Delete(ctx, sb.ID); err != nil {
			t.Fatal(err)
		}
		sb.Metadata[sandboxBackendKey] = backend
		if err := o.st.Put(ctx, sb); err != nil {
			t.Fatal(err)
		}
		key := mintTestAPIKey(t, sb.APISecret)
		if _, err := o.Connect(ctx, sb.ID, key, "", api.ConnectOptions{}); !errors.Is(err, api.ErrBadRequest) {
			t.Fatalf("unsupported backend %s resume = %v", backend, err)
		}
	}
}

func TestBackendClusterSelectionAndHookRevalidation(t *testing.T) {
	for _, selected := range []string{"cloud-hypervisor", "firecracker", "qemu"} {
		t.Run(selected, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Sandbox.Backend = "firecracker"
			cfg.Sandbox.TimeoutSec = 600
			o, ctx := newAsyncConnectTestOrchestrator(t, cfg, &countingLauncher{})
			_, _, fingerprint := allowlistedBuildIdentity(t, o)
			cmd := clusterCreateCommand(fingerprint, "mixed-cluster")
			cmd.Backend = "cloud-hypervisor"
			o.SetExtensionHooks(sandboxHookFunc(func(_ context.Context, op *conductorextension.SandboxOperation) error {
				op.Create.Backend = selected
				op.Create.Metadata = map[string]string{sandboxcfg.NsResource: staticFirecrackerResource}
				return nil
			}), nil)
			ack := o.HandleCommand(ctx, cmd)
			if selected == "qemu" {
				if ack.Status != routesync.AckRejected || ack.HTTPStatus != 400 {
					t.Fatalf("invalid backend ACK = %+v", ack)
				}
				return
			}
			if ack.Status != routesync.AckAccepted {
				t.Fatalf("cluster backend ACK = %+v", ack)
			}
			sb := waitForSandbox(t, o, ctx, cmd.SID, func(sb *types.Sandbox) bool { return sb.State == types.StateRunning }, "cluster backend running")
			if sandboxBackend(sb) != selected || cmd.Backend != "cloud-hypervisor" {
				t.Fatalf("cluster backend selection = %q, original command = %q", sandboxBackend(sb), cmd.Backend)
			}
		})
	}
}
