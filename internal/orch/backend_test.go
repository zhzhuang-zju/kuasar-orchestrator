package orch

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	conductorextension "github.com/kuasar-sandbox/orchestrator/app/conductor/extension"
	"github.com/kuasar-sandbox/orchestrator/internal/api"
	"github.com/kuasar-sandbox/orchestrator/internal/config"
	"github.com/kuasar-sandbox/orchestrator/internal/nodepath"
	"github.com/kuasar-sandbox/orchestrator/internal/sandboxcfg"
	"github.com/kuasar-sandbox/orchestrator/internal/types"
	"github.com/kuasar-sandbox/orchestrator/internal/workspace"
)

func TestBackendMetadataDefault(t *testing.T) {
	for _, sb := range []*types.Sandbox{nil, {}, {Metadata: map[string]string{}}, {Metadata: map[string]string{sandboxBackendKey: ""}}} {
		if got := sandboxBackend(sb); got != "cloud-hypervisor" {
			t.Fatalf("legacy backend = %q, want cloud-hypervisor", got)
		}
	}
}

func TestBackendCreatePersistsNodeSelection(t *testing.T) {
	for _, backend := range []string{"", "cloud-hypervisor", "firecracker"} {
		t.Run("backend="+backend, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Sandbox.Backend = backend
			o, ctx := newAsyncConnectTestOrchestrator(t, cfg, &countingLauncher{})
			req := createRequestFixture(t, o, "1")
			// Firecracker requires explicitly static memory, not the node's
			// smaller default allocatable memory budget.
			req.Metadata[sandboxcfg.NsResource] = `{"capacity":{"cpu":2,"memory":"2GiB"},"allocatable":{"memory":"2GiB"}}`
			original := cloneStringMap(req.Metadata)
			created, err := o.Create(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			want := cfg.Sandbox.EffectiveBackend()
			if created.Metadata[sandboxBackendKey] != want {
				t.Fatalf("accepted backend = %q, want %q", created.Metadata[sandboxBackendKey], want)
			}
			if !reflect.DeepEqual(req.Metadata, original) {
				t.Fatal("Create mutated caller metadata")
			}
			created.Metadata[sandboxBackendKey] = "caller-mutated"
			req.Metadata[sandboxBackendKey] = "caller-mutated"
			current := waitForSandbox(t, o, ctx, created.ID, func(sb *types.Sandbox) bool {
				return sb.State == types.StateRunning
			}, "backend create running")
			if current.Metadata[sandboxBackendKey] != want {
				t.Fatalf("durable backend = %q, want %q", current.Metadata[sandboxBackendKey], want)
			}
			if _, err := o.launches.Wait(ctx, created.ID); err != nil {
				t.Fatal(err)
			}
			// Bypass the hot cache: launch construction must recover the
			// backend from the durable row, not the caller's returned map.
			restarted := &Orchestrator{cfg: cfg, st: o.st, vs: stubVS{}, executables: o.executables}
			spec, _, found, err := restarted.LaunchSpecFor(ctx, "sandbox:"+created.ID)
			if err != nil || !found || spec == nil {
				t.Fatalf("durable LaunchSpecFor = %+v, %t, %v", spec, found, err)
			}
			if !hasArgPair(spec.Args, "--backend", want) {
				t.Fatalf("durable launch backend missing: %v", spec.Args)
			}
			if ok, err := o.Kill(ctx, created.ID, req.APIKey); err != nil || !ok {
				t.Fatalf("Kill = %t, %v", ok, err)
			}
			waitForSandboxAbsent(t, o, ctx, created.ID, "backend create deletion")
		})
	}
}

func TestBackendMetadataRejectsCallerAndHook(t *testing.T) {
	for _, backend := range []string{"", "firecracker"} {
		for _, carrier := range []string{"caller", "hook"} {
			for _, value := range []string{"", "cloud-hypervisor", "firecracker"} {
				t.Run(backend+"/"+carrier+"/value="+value, func(t *testing.T) {
					cfg := &config.Config{}
					cfg.Sandbox.Backend = backend
					lc := &countingLauncher{}
					o, ctx := newAsyncConnectTestOrchestrator(t, cfg, lc)
					vs := &resourcePreflightVS{}
					o.vs = vs
					req := createRequestFixture(t, o, "2")
					called := false
					o.SetExtensionHooks(sandboxHookFunc(func(_ context.Context, operation *conductorextension.SandboxOperation) error {
						called = true
						if _, present := operation.Create.Metadata[sandboxBackendKey]; present {
							t.Fatal("Hook received writable backend metadata")
						}
						operation.Create.Metadata[sandboxBackendKey] = value
						return nil
					}), nil)
					if carrier == "caller" {
						req.Metadata[sandboxBackendKey] = value
					}
					original := cloneStringMap(req.Metadata)
					created, err := o.Create(ctx, req)
					if !errors.Is(err, api.ErrBadRequest) || created != nil || !strings.Contains(err.Error(), "backend is node-managed") {
						t.Fatalf("backend override = %+v, %v", created, err)
					}
					if called != (carrier == "hook") || !reflect.DeepEqual(req.Metadata, original) {
						t.Fatalf("Hook called = %t; caller metadata = %v, want %v", called, req.Metadata, original)
					}
					assertNoFreshLaunchSideEffects(t, o, ctx, vs, lc)
				})
			}
		}
	}
}

func TestBackendLaunchSpec(t *testing.T) {
	for _, test := range []struct {
		name, nodeBackend, storedBackend, binary, want string
	}{
		{name: "legacy default", want: "cloud-hypervisor"},
		{name: "explicit cloud hypervisor", nodeBackend: "cloud-hypervisor", storedBackend: "cloud-hypervisor", binary: "/opt/firecracker", want: "cloud-hypervisor"},
		{name: "firecracker default binary", nodeBackend: "firecracker", storedBackend: "firecracker", want: "firecracker"},
		{name: "firecracker custom binary", nodeBackend: "firecracker", storedBackend: "firecracker", binary: "/opt/firecracker", want: "firecracker"},
		{name: "durable selection not node default", nodeBackend: "firecracker", want: "cloud-hypervisor"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Sandbox.Backend, cfg.Sandbox.FirecrackerBinary = test.nodeBackend, test.binary
			cfg.Paths.RunRoot, cfg.Paths.BaseRoot = "/tmp/backend-run", "/tmp/backend-base"
			o := testOrchCfg(t, cfg)
			o.vs = stubVS{}
			sb := backendSandboxFixture(t, cfg, types.StateRunning, test.storedBackend)
			if err := o.st.Put(context.Background(), sb); err != nil {
				t.Fatal(err)
			}
			spec, pidFile, found, err := o.LaunchSpecFor(context.Background(), "sandbox:"+sb.ID)
			if err != nil || !found || spec == nil {
				t.Fatalf("LaunchSpecFor = %+v, %t, %v", spec, found, err)
			}
			if spec.Exec != o.executables.SandboxCtl() || spec.Workdir != sb.RunDir || pidFile != sb.PidFile() {
				t.Fatalf("launch identity = %+v, pidfile %q", spec, pidFile)
			}
			for key, value := range map[string]string{
				"--backend": test.want, "--sandbox-id": sb.ID, "--path-id": sb.ID,
				"--config":    o.sandboxConfigPath(sb),
				"--run-root":  nodepath.SandboxRunRoot(cfg.Paths.RunRoot),
				"--base-root": nodepath.SandboxBaseRoot(cfg.Paths.BaseRoot),
			} {
				if !hasArgPair(spec.Args, key, value) {
					t.Fatalf("missing %s %q: %v", key, value, spec.Args)
				}
			}
			if test.want == "cloud-hypervisor" {
				if !hasArg(spec.Args, "--enable-checkpoint") || !hasArgPair(spec.Args, "--retain-instance", sb.InstanceID) || !hasArgPair(spec.Args, "--run-id", sb.RunID) || hasArg(spec.Args, "--firecracker-binary") {
					t.Fatalf("Cloud Hypervisor launch contract changed: %v", spec.Args)
				}
			} else {
				// --run-id also requests a checkpoint identity in sandbox-ctl;
				// omitting just --enable-checkpoint still breaks real FC cold boot.
				for _, arg := range spec.Args {
					for _, forbidden := range []string{"--enable-checkpoint", "--retain-instance", "--run-id"} {
						if arg == forbidden || strings.HasPrefix(arg, forbidden+"=") {
							t.Fatalf("Firecracker received checkpoint flag %q: %v", arg, spec.Args)
						}
					}
				}
				if test.binary == "" {
					if hasArg(spec.Args, "--firecracker-binary") {
						t.Fatalf("unexpected Firecracker binary override: %v", spec.Args)
					}
				} else if !hasArgPair(spec.Args, "--firecracker-binary", test.binary) {
					t.Fatalf("missing Firecracker binary override: %v", spec.Args)
				}
			}
		})
	}
}

func TestFirecrackerCreateRejectsUnsupportedLifecycle(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*api.CreateReq)
	}{
		{"snapshot template", func(req *api.CreateReq) {
			req.TemplateID = types.TemplateID{Profile: types.ProfileBare, Kind: types.KindSnp, Ref: "manifest://" + strings.Repeat("a", 64)}.String()
		}},
		{"sandbox template", func(req *api.CreateReq) {
			req.TemplateID = types.TemplateID{Profile: types.ProfileBare, Kind: types.KindSbx, Ref: "manifest://" + strings.Repeat("a", 64)}.String()
		}},
		{"auto reset", func(req *api.CreateReq) { req.AutoReset = true }},
		{"idle auto pause", func(req *api.CreateReq) { idle := int64(60); req.IdleTimeout = &idle }},
		{"workspaces", func(req *api.CreateReq) { req.Workspaces = []workspace.Entry{{}} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Sandbox.Backend = "firecracker"
			lc := &countingLauncher{}
			o, ctx := newAsyncConnectTestOrchestrator(t, cfg, lc)
			vs := &resourcePreflightVS{}
			o.vs = vs
			req := createRequestFixture(t, o, "3")
			test.edit(&req)
			created, err := o.Create(ctx, req)
			if !errors.Is(err, api.ErrBadRequest) || created != nil || !strings.Contains(err.Error(), "firecracker requires an image template") {
				t.Fatalf("unsupported Create = %+v, %v", created, err)
			}
			assertNoFreshLaunchSideEffects(t, o, ctx, vs, lc)
		})
	}
}

func TestFirecrackerRejectsUnsupportedOperations(t *testing.T) {
	cfg := &config.Config{}
	cfg.Sandbox.Backend = "firecracker"
	lc := &countingLauncher{}
	o, ctx := newAsyncConnectTestOrchestrator(t, cfg, lc)
	vs := &resourcePreflightVS{}
	o.vs = vs
	req := createRequestFixture(t, o, "4")
	for _, test := range []struct {
		name string
		call func() error
	}{
		{"pause snapshot", func() error {
			_, err := o.Pause(ctx, "missing", req.APIKey, sandboxcfg.CaptureRequest{Kind: types.CaptureSnapshot})
			return err
		}},
		{"pause without memory", func() error {
			_, err := o.Pause(ctx, "missing", req.APIKey, sandboxcfg.CaptureRequest{Kind: types.CaptureSandbox})
			return err
		}},
		{"reset", func() error { return o.Reset(ctx, "missing", req.APIKey) }},
		{"register build", func() error { _, err := o.RegisterBuild(ctx, req.APIKey, api.RegisterSpec{}); return err }},
		{"trigger build", func() error {
			return o.TriggerBuild(ctx, req.APIKey, "missing", "missing", api.TriggerSpec{}, api.BuildAuth{})
		}},
		{"export", func() error { _, err := o.ExportSandbox(ctx, req.APIKey, "missing", false, false, ""); return err }},
		{"export template", func() error { _, err := o.ExportSandbox(ctx, req.APIKey, "missing", true, false, ""); return err }},
		{"import", func() error {
			_, err := o.ImportSandbox(ctx, req.APIKey, "invalid-token", "", api.ImportOptions{})
			return err
		}},
		{"attach workspace", func() error { return o.AttachWorkspace(ctx, "missing", req.APIKey, "data", nil) }},
		{"export workspace", func() error { _, err := o.ExportWorkspace(ctx, "missing", req.APIKey, "data"); return err }},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.call()
			if !errors.Is(err, api.ErrBadRequest) || !strings.Contains(err.Error(), "firecracker does not support") {
				t.Fatalf("unsupported operation = %v", err)
			}
			assertNoFreshLaunchSideEffects(t, o, ctx, vs, lc)
			var builds int
			err = o.st.RangeBuilds(ctx, func(*types.Build) error { builds++; return nil })
			if err != nil || builds != 0 {
				t.Fatalf("unsupported operation persisted %d builds: %v", builds, err)
			}
		})
	}
}

func TestBackendRecoveryRejectsActiveSwitchBeforeSideEffects(t *testing.T) {
	for _, backend := range []string{"", "cloud-hypervisor", "firecracker"} {
		for _, state := range []types.State{types.StateStarting, types.StateRunning, types.StatePausing, types.StatePaused} {
			t.Run("stored="+backend+"/"+string(state), func(t *testing.T) {
				cfg := &config.Config{}
				cfg.Sandbox.Backend = "firecracker"
				if backend == "firecracker" {
					cfg.Sandbox.Backend = "cloud-hypervisor"
				}
				o := testOrchCfg(t, cfg)
				lc, vs := &reconcileLauncher{}, &reconcileVS{}
				o.lc, o.vs = lc, vs
				sb := backendSandboxFixture(t, cfg, state, backend)
				ctx := context.Background()
				if err := o.st.Put(ctx, sb); err != nil {
					t.Fatal(err)
				}
				before, err := o.st.Get(ctx, sb.ID)
				if err != nil {
					t.Fatal(err)
				}
				if err := o.ReconcileSandboxes(ctx); err == nil || !strings.Contains(err.Error(), "cannot switch sandbox.backend") || !strings.Contains(err.Error(), sb.ID) {
					t.Fatalf("backend switch recovery = %v", err)
				}
				after, err := o.st.Get(ctx, sb.ID)
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Fatalf("failed recovery changed durable sandbox: %+v, %v", after, err)
				}
				if lc.listCalls != 0 || len(lc.stopped) != 0 || len(lc.reset) != 0 || lc.reloads != 0 || vs.attaches != 0 || len(vs.detached) != 0 {
					t.Fatal("backend switch touched launcher or network before rejection")
				}
			})
		}
	}
}

func TestBackendRecoveryAllowsMatchingAndTerminalSandboxes(t *testing.T) {
	for _, backend := range []string{"", "cloud-hypervisor", "firecracker"} {
		for _, state := range []types.State{types.StateStarting, types.StateRunning, types.StatePausing, types.StatePaused, types.StateDead, types.StateDeleting} {
			t.Run("stored="+backend+"/"+string(state), func(t *testing.T) {
				cfg := &config.Config{}
				cfg.Sandbox.Backend = backend
				// Terminal rows do not prevent selecting a different backend.
				if state == types.StateDead || state == types.StateDeleting {
					cfg.Sandbox.Backend = "firecracker"
					if backend == "firecracker" {
						cfg.Sandbox.Backend = "cloud-hypervisor"
					}
				}
				o := testOrchCfg(t, cfg)
				sb := backendSandboxFixture(t, cfg, state, backend)
				ctx := context.Background()
				if err := o.st.Put(ctx, sb); err != nil {
					t.Fatal(err)
				}
				if err := o.validateBackendRecovery(ctx); err != nil {
					t.Fatalf("compatible recovery rejected: %v", err)
				}
			})
		}
	}
}

func TestFirecrackerRecoveryRejectsPendingReset(t *testing.T) {
	cfg := &config.Config{}
	cfg.Sandbox.Backend = "firecracker"
	o := testOrchCfg(t, cfg)
	sb := backendSandboxFixture(t, cfg, types.StateDead, "cloud-hypervisor")
	sb.ResetState, sb.ResetID = "pending", "reset-backend"
	ctx := context.Background()
	if err := o.st.Put(ctx, sb); err != nil {
		t.Fatal(err)
	}
	if err := o.validateBackendRecovery(ctx); err == nil || !strings.Contains(err.Error(), "pending sandbox reset "+sb.ID) {
		t.Fatalf("pending reset recovery = %v", err)
	}
}

func TestFirecrackerBuildRecovery(t *testing.T) {
	for _, backend := range []string{"", "firecracker"} {
		for _, state := range []types.BuildState{types.BuildRegistered, types.BuildWaiting, types.BuildBuilding, types.BuildReady, types.BuildError} {
			t.Run(backend+"/"+string(state), func(t *testing.T) {
				cfg := &config.Config{}
				cfg.Sandbox.Backend = backend
				o := testOrchCfg(t, cfg)
				key := strings.Repeat("a", 64)
				build := &types.Build{BuildID: "backend-build", TemplateID: "backend-template", Profile: types.ProfileBare,
					Status: state, APISecret: deriveTestAPISecret(t, key), ManifestKey: key, CreatedUnix: 1}
				ctx := context.Background()
				if err := o.st.PutBuild(ctx, build); err != nil {
					t.Fatal(err)
				}
				err := o.validateBackendRecovery(ctx)
				blocked := backend == "firecracker" && state != types.BuildReady && state != types.BuildError
				if blocked {
					if err == nil || !strings.Contains(err.Error(), "nonterminal build "+build.BuildID) {
						t.Fatalf("nonterminal build recovery = %v", err)
					}
				} else if err != nil {
					t.Fatalf("compatible build recovery rejected: %v", err)
				}
			})
		}
	}
}

func backendSandboxFixture(t *testing.T, cfg *config.Config, state types.State, backend string) *types.Sandbox {
	t.Helper()
	key := strings.Repeat("a", 64)
	sb := &types.Sandbox{
		ID: "backend-sandbox", Profile: types.ProfileBare, State: state,
		TemplateID: types.TemplateID{Profile: types.ProfileBare, Kind: types.KindImg, Ref: "manifest://" + key}.String(),
		RunID:      "sr-00000000-0000-7000-8000-000000000001", InstanceID: "backend-instance",
		RunDir: nodepath.SandboxRunDir(cfg.Paths.RunRoot, "backend-sandbox"), BaseDir: nodepath.SandboxBaseDir(cfg.Paths.BaseRoot, "backend-sandbox"),
		APISecret: deriveTestAPISecret(t, key), ManifestKey: key, CreatedUnix: 1,
	}
	if backend != "" {
		sb.Metadata = map[string]string{sandboxBackendKey: backend}
	}
	if state == types.StateStarting {
		sb.LaunchMode = types.LaunchImage
	}
	if state == types.StatePaused || state == types.StatePausing {
		sb.ResumeSource = types.ResumeSource{Kind: types.ResumeSourceSnapshot, Ref: "manifest://" + strings.Repeat("b", 64), SandboxRef: "manifest://" + strings.Repeat("c", 64)}
	}
	if state == types.StateDead {
		sb.DeadUnix = 2
		sb.DeadToken = "backend-terminal"
		sb.DeadRunID = sb.RunID
		sb.RunID, sb.RunDir = "", ""
	}
	materializeTestSandboxCredentials(t, sb)
	return sb
}
