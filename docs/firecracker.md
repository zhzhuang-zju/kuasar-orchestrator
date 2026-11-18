# Experimental Firecracker cold-image backend

## Per-request selection validation

The per-request implementation was exercised in the ARM64 QEMU laboratory on
2026-11-17, before the public selector migrated to reserved metadata. One
Conductor with a Cloud Hypervisor default accepted explicit
`backend: cloud-hypervisor` and `backend: firecracker` Create requests. Both
VMs ran concurrently, and both passed authenticated Proxy native exec and guest
file write/read checks. An invalid backend was rejected with HTTP 400.

With both VMs running, the node default was changed to Firecracker and Conductor
was restarted. Both original sandboxes remained running and passed the same
access checks. Both were then deleted,
with GET returning 404, no remaining runner/VMM processes, and all 64 switch
ports available again. This validates standalone mixed-backend recovery, not
an end-to-end multi-node cluster deployment.

Affected API, extension, config, lifecycle, Builder, runner, Router, Registry and
route-protocol package tests passed. Backend-focused race tests also passed.
Cluster propagation is covered by automated tests; no real cluster was deployed.

The subsequent metadata-input migration is covered by automated tests only;
the historical live checks above used the now-obsolete top-level selector.
No worker deployment was changed or live test repeated for this migration.

The lab now uses transient `kuasar-per-request-conductor` and
`kuasar-per-request-proxy` services plus the existing Connector. Its default is
Firecracker and its runner capacity is two. Verification sandboxes were deleted;
earlier PicoD sandbox/token examples may have expired. These are handoff facts,
not guarantees of current liveness or reboot persistence.

Conductor remains the sandbox manager. It assigns an authenticated task to a
systemd-managed runner, which starts `sandbox-ctl run --backend firecracker`.
The enhanced runtime configures Firecracker through its Unix HTTP API and uses
the existing guest launch, native exec, forwarding, readiness, and cleanup
protocols. Cloud Hypervisor remains the default backend.

## Scope

This is a working **cold-image create / native exec / delete** implementation,
not complete VMM feature parity. Create accepts reserved metadata
`"kuasar-sandbox.backend": "{\"name\":\"firecracker\"}"`, or the same object with
`name` set to `cloud-hypervisor` (CH), case-sensitively. Only omission selects
`sandbox.backend` (CH by default). A supplied selector must be a JSON string
containing exactly one object with a supported nonempty string `name`; null,
empty values, wrong types, duplicate/unknown fields and trailing JSON fail.
The obsolete public top-level `backend` field is rejected, including empty/null.
There is no backend header alias or template/Build backend default.
A node can host both backends;
selection does not bypass authentication, admission or capability validation.
Create Hooks receive the resolved selection in typed `SandboxCreateRequest.Backend`
and may change it before core revalidation. The metadata selector is consumed
before Hooks; Hooks cannot reintroduce it as a second authority.
Cluster Create carries the normalized typed field
through Router, Registry reservation and node-link to the selected node.

The final selection is resolved and fixed at Create acceptance in core-owned
durable metadata, not caller-writable metadata. Legacy rows without it mean CH.
Changing the node default alone does not block recovery or change existing
sandboxes' backends. Build always uses CH, independently of the node default;
supported CH artifact imports are pinned to CH as well.

Firecracker does not use the Cloud Hypervisor pmem, vhost-user, patched UFFD,
or balloon-control integration. The adapter exposes the runtime bundle as a
read-only virtio root disk and materializes logical workload block views into
disposable raw disks. A versioned guest hello confirms that guest init supports
the shifted block layout and invalidates caches before mounting those disks.
Writable disks are deleted after VMM termination; they are **not** export or
snapshot sources. Materialization consumes local disk and startup time.

The following are explicitly unsupported for this backend:

- Snapshot/restore, pause/resume, reset, sandbox import/export and migration.
- Builder execution, workspace-backed launches, dynamic disk slots and explicit
  persistent writable DIFF paths.
- Auto-reset and idle auto-pause. FC TTL expiry performs Delete rather than
   attempting unsupported Pause/capture, regardless of `autoPauseMemory`.
- Dynamic resource control, memory ballooning, and manual DISCARD policy.
- Encrypted local runtime storage.

Unsupported operations fail rather than falling back to Cloud Hypervisor.
The existing Cloud Hypervisor path retains checkpoint arguments and behavior.

## Required source changes

Build matching editable checkouts of `orchestrator`, `sandboxer`, and
`guest-runtime`, together with their selected `accelerator` and `connector`
dependencies. The adapter changes span **all three repositories**; rebuilding
only `node-ctl` is insufficient. No module-cache source was modified.

The sibling sandboxer adds `--backend` and `--firecracker-binary`, API-driven
VMM boot, regular-disk materialization, persistent TAP identification, and
capability rejection. Guest runtime adds the validated Firecracker boot
layout/hello and pre-mount cache invalidation. The runner retains trusted
cgroup/readiness descriptor ownership and validates backend arguments.

Use a Firecracker-compatible kernel with built-in virtio-mmio/block/net/vsock,
EROFS, overlay/ext4, and the appropriate serial console. Guest-runtime kernel
presets include the additional Firecracker console/mmio requirements. The
existing released ARM64 kernel worked in the local test; the modified presets
have not themselves been rebuilt or validated on x86_64.

## Node configuration

Use the ordinary protected Conductor configuration and add:

```yaml
sandbox:
  backend: firecracker
  firecracker_binary: /opt/kuasar/bin/firecracker
  resources:
    capacity: {cpu: 1, memory: 384MiB}
    allocatable: {cpu: 1, memory: 384MiB}
```

CPU capacity must be 1–32. Memory must be a positive whole MiB, with explicit
`allocatable.memory` equal to `capacity.memory`; omit startup memory or set it
equal to capacity. FC is currently static-only: if node `resource_listen.enabled`
is true, FC requests are rejected even with static memory settings. CH requests
remain available. Resource validation follows the final selected backend; selection
does not rewrite elastic CH defaults into FC-compatible resources. Supply the
explicit allocation in node defaults or the request's resource patch (the omitted
node value normally inherits 256 MiB).

For example, on a CH-default node with `resource_listen` disabled:

```json
{
   "templateID": "<canonical-image-template-id>",
   "metadata": {
      "kuasar-sandbox.backend": "{\"name\":\"firecracker\"}",
      "kuasar-sandbox.resource": "{\"capacity\":{\"cpu\":1,\"memory\":\"384MiB\"},\"allocatable\":{\"memory\":\"384MiB\"}}"
   }
}
```

The guest kernel/runtime and ready canonical image template must match the
architecture. Keep ordinary manifest-key/API authentication and Proxy enabled.
The Firecracker process is created directly in the delegated VMM cgroup and
the TAP namespace. TAP FD handoff must identify a persistent interface;
Firecracker opens the named interface after the runtime releases its queue.

## Earlier cold-backend validation

The observations below predate per-request selection; they do not establish live
validation of mixed-backend operation or the new cluster/Hook propagation.

The [ARM64 QEMU lab](qemu-lab.md) was extended with an isolated adapter install
under `/opt/kuasar-adapter` inside Debian. Original release files under
`/opt/kuasar` were preserved. Modified ARM64 `node-ctl`, `sandbox-ctl` and
guest init were cross-built from the sibling working trees. The guest init
replaced the old init in a repacked, digest-marked EROFS runtime bundle.

Observed with Firecracker **v1.18.0**, Debian ARM64 kernel
**6.12.116+deb13-arm64**, QEMU **6.2 TCG**, and the existing released guest
kernel:

1. Three Firecracker-backed sandboxes reached `running`.
2. Authenticated Proxy native exec returned `hello-from-kuasar` and
   `Linux aarch64` with exit status 0.
3. Guest file write/read returned `writable-ok`.
4. Delete returned 204; bounded polling subsequently returned 404.
5. Runner units and runtime/VMM processes disappeared; all 64 switch ports
   were available again.
6. Cloud Hypervisor passed the same Create/exec/write/read/Delete test using
   the **modified** conductor, runner, sandbox runtime and guest init.

These are functional observations under emulation, not benchmarks. The tested
profile is bare Alpine ARM64; E2B/Envd, guest Internet egress, additional disks,
x86_64 runtime boot, and a production deployment are not validated here.

Focused Go tests passed in configuration, Conductor, runner, sandbox CLI/SDK,
guestlink, TAP helpers, runtime facade, and guest init. Tests cover selector
validation, CH defaults, unsupported Firecracker capabilities, durable backend
identity/recovery, block mapping/materialization, guest cache invalidation,
and the readiness handoff. Focused backend configuration/Conductor/runner
tests also passed with Go's race detector.

The adapter laboratory retains protected credentials, configs and verifier
outside the source checkout under `/var/lib/kuasar-qemu-lab`. Backend tests use
separate `/run/kuasar-adapter` and `/var/lib/kuasar-adapter` roots. No real
test sandboxes are intentionally left running after verification.

At handoff, `kuasar-adapter-conductor`, `kuasar-adapter-proxy`, and
`kuasar-connector-lab` are active in Debian; the adapter Conductor selects
Firecracker. The original release Conductor/Proxy are stopped to avoid port
conflicts. Host loopback forwards remain API 18083, Proxy 18084, and SSH 19222.
Services are transient and do not constitute reboot-safe installation.

For another test, SSH into Debian and run:

```sh
sudo env KUASAR_BIN=/opt/kuasar-adapter/bin \
   KUASAR_LAB=/opt/kuasar-adapter/lab \
   NODE_CTL_SOCKET=/run/kuasar-adapter/node-ctl.socket \
   sh /opt/kuasar-adapter/lab/create-sandbox.sh
sudo env KUASAR_BIN=/opt/kuasar-adapter/bin \
   KUASAR_LAB=/opt/kuasar-adapter/lab \
   /opt/kuasar-adapter/lab/verify-sandbox
```

The second command verifies readiness, command execution and writable storage,
then deletes the sandbox. Secrets remain in root-readable guest files.
The local build workspace is `/var/lib/kuasar-qemu-lab/adapter.go.work`; it
resolves unpublished coordinated v0.2.0 dependencies to the sibling sources.
Without that workspace, editor module diagnostics may still report unavailable
dependencies. Repository dependency versions were not rewritten.