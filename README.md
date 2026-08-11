# Live migration for Firecracker microVMs

Firecracker deliberately does not support live migration — the maintainers
[closed the discussion](https://github.com/firecracker-microvm/firecracker/discussions/3119)
with "snapshot and restore is enough for our use cases." This project makes it
support live migration anyway: a ~250-line patch to the VMM adds a pause-free
snapshot primitive, and a Go control plane orchestrates true pre-copy
migration between two hosts, moving a running microVM — open TCP connections
and all — with a **pause→resume blackout of ~10–25ms**, verified end to end
by an external prober and a guest that cryptographically self-checks every
page of its memory.

```
macOS (Apple Silicon, M3+)
└─ Lima VM (Ubuntu 24.04, nested virtualization → /dev/kvm)
   └─ Docker network 172.30.0.0/24 (one L2 segment) + shared storage volume
      ├─ host-a   migrated agent ── firecracker (patched) ── tap0 ─┐
      ├─ host-b   migrated agent ── firecracker (patched) ── tap0 ─┼── guest 172.30.0.50
      └─ client   fcprobe: sub-ms UDP probes, TCP stream, verdicts ┘    (keeps MAC+IP across hosts)
```

## Why this shape

- **The gap in Firecracker**: every stock snapshot — even a diff — requires
  pausing the vCPUs, so "pre-copy" with stock APIs pauses the guest once per
  round. The patch adds `SnapshotType::DiffLive`: dump the dirty pages while
  the vCPUs keep running. Guest writes racing the dump are covered by KVM's
  fetch-and-clear dirty log (torn pages are always re-marked and re-copied);
  VMM-side writes can't race it at all (device emulation runs on the same
  thread as the dump). Only the *final* round pauses, and by then the
  residual dirty set is a few hundred KiB.
- **Bounded stalls**: a live dump occupies the VMM event loop, which also
  services virtio. `live_max_bytes` caps each dump; over-budget pages are
  re-marked in the dirty bitmap, which doubles as the drain backlog. Draining
  the backlog needs **no** KVM dirty-log fetch (each fetch costs a stage-2
  TLB flush — severely amplified under nested virtualization), so the agent
  drains in 1MiB slices with single-digit-ms stalls and syncs with KVM only
  when the backlog runs low.
- **The network follows the VM**: the guest owns its MAC+IP directly on the
  shared L2 (each host bridges the tap with its uplink). On resume, the
  target agent emits gratuitous ARP; every bridge on the path re-learns, and
  established TCP connections simply keep flowing — the demo holds a
  streaming TCP session open across the migration.
- **Storage is shared** (`/srv/fcmig`, the NFS/SAN stand-in of a real
  cluster), the standard live-migration storage assumption: no disk data
  moves in the hot path.
- **Honest measurement**: `fcprobe` hammers the guest with 1ms UDP probes
  and reports *client-observed* blackout (longest run of probes never
  answered) next to the agents' pause→resume timestamps (directly comparable
  — all containers share one kernel, hence one CLOCK_MONOTONIC). Degradation
  (probes answered slower than 5ms — pre-copy contention, post-restore
  fault-in) is reported separately and never conflated with blackout. The
  guest's scribbler continuously rewrites and verifies self-describing
  memory pages, so a single torn or lost page anywhere in the pipeline shows
  up as a nonzero `integrity_errors`.

## Quickstart

Requires: Apple Silicon M3+ on macOS 15+, [Lima](https://lima-vm.io) ≥ 1.0,
Go ≥ 1.23. Everything else (Docker, Rust, Firecracker itself) is provisioned
inside the Lima VM.

```console
$ make vm       # one-time: create the nested-virt Linux VM
$ make setup    # build Firecracker v1.16.1 from source + patch, kernel, rootfs
$ make demo     # boot vm0 on host-a, live-migrate to host-b under probing
$ make bench    # N ping-pong migrations → bench-results/blackout.csv
$ make hostile  # crank the guest's dirty rate, watch auto-converge
```

`make demo` output (one run, on the laptop under nested virt):

```
agent  pause→resume    :    15.67 ms  (VM provably not executing)
client bracketing probe:   371.27 ms  (unanswered run overlapping pause→resume)
guest integrity        : 0 errors over 65284 pages verified
blackout budget 30ms → FAIL
```

The agent figure is the downtime, and it meets the budget. The client figure
does not, and `fcprobe` reports the stricter of the two, so the laptop prints
`FAIL` — that is accurate and deliberately left in. The difference between
the two is the post-restore fault-in tail explained under
[Measured](#measured); it is a property of running this nested on a laptop,
not of the migration itself.

## Layout

| Path | What it is |
|---|---|
| `patches/0001-*.patch` | The Firecracker change: `DiffLive` snapshots + budget + backlog (applies to v1.16.1) |
| `cmd/migrated` | Migration API daemon, one per host (`POST /vms`, `DELETE /vms/{id}`, `POST /vms/{id}/migrate`, SSE events) |
| `cmd/guestd` | Guest workload: UDP echo, TCP counter stream, self-verifying memory scribbler |
| `cmd/fcprobe` | Prober / bench client — the adversarial measurement side |
| `internal/` | Firecracker API client, sparse-extent streaming, GARP, vCPU throttler, timeline |
| `deploy/` | The two "hosts" + client (compose); bridge/tap plumbing in the entrypoint |
| `scripts/` | Source build, rootfs build, demo/bench drivers |
| `lima/fcmig.yaml` | The environment answer: KVM on a MacBook via nested virtualization |
| `docs/` | Architecture deep-dive, measurement methodology, AI-usage log |

## Design notes you may be looking for

- **Migration flow**: prepare target (spawn VMM early — off the critical
  path) → stream base image, paced → budgeted DiffLive drain rounds, paced,
  with breathers → pause → final Diff (residual + vCPU/device state) →
  restore + resume on target → gratuitous ARP → kill source. Failure at any
  point rolls back: the source resumes and stays authoritative.
- **Auto-converge**: if the drain provably can't converge (it has run longer
  than the guest's whole memory would take at the transfer pace), the agent
  duty-cycles SIGSTOP/SIGCONT on the *vCPU threads only* — the VMM event
  loop and the drain keep running at full speed, exactly the semantics of
  QEMU's vCPU throttling.
- **Ping-pong**: a restored VM's memory file is a complete image as of the
  final pause, so it becomes the base for the next migration — `make bench`
  bounces the VM A→B→A→… without ever re-sending a full copy cold.
## Measured

Four consecutive migrations on the laptop (nested virt, 256 MiB guest), one
after another on a quiesced host:

```
agent  pause→resume    :  7.9 / 13.6 / 17.5 / 17.7 ms   (VM provably not executing)
guest integrity        :  0 errors, every run           (self-verified pages)
guest survival         :  4 / 4                         (TCP session intact across each switch)
client switchover gap  :  179-324 ms                    (see the fault-in tail below)
```

The **agent-measured** blackout — recorded inside the VMM around the actual
KVM pause/resume — is the trustworthy figure and sits within the 30 ms budget
whenever the host has CPU to give. It is immune to *guest*-level jitter, but
not to host starvation: pause→resume is wall-clock time that includes
host-scheduled work, so on a laptop swapping hard (load 6+, 12% free RAM) the
same build reported 180 ms. Quiesce the host before believing any number here.
The client-observed number is larger — ~180–600 ms, scaling with the number
of pre-copy rounds — and that gap is real
rather than noise: probed at rest with no migration in flight, this guest
goes unanswered for at most ~13 ms. It is the post-restore fault-in tail. The
VM has already resumed, but its RAM is a file it hasn't touched yet, and
under nested virt each of those ~65k first-touch faults costs ~10× what it
would on bare metal, so the guest is executing without being able to answer.
Because `fcprobe` reports the stricter of the two figures, `make demo` on a
laptop prints `blackout budget 30ms → FAIL` — accurate, and left unsoftened.
Bare metal is expected to collapse that tail and converge the two numbers;
that expectation is predicted, not yet measured. See
[docs/measurements.md](docs/measurements.md) for the methodology, the honest
brownout accounting, and why the laptop and bare-metal numbers differ.

## Known limitations

- **Pre-copy brownout.** During the copy rounds the guest keeps executing
  (its counter never stops, TCP survives) but its network I/O is intermittently
  delayed, because the memory dump shares the VMM event-loop thread with the
  virtio device — ~10× amplified under nested virt. Fix: run the dump on a
  separate thread (the guest memory is `Arc`-shared) — noted as future work.
- **Guest kernel panic on restore, under host starvation.** The guest can
  panic on the *target* just after restore — always in the timer softirq path
  (`call_timer_fn` → `__run_timers` → `run_timer_softirq`), memory content
  verified intact. Two measurements narrow it down. It is **not** the
  `DiffLive` patch: six fresh-boot migrations with `live_rounds=false` (stock
  paused diff, no patch in the path) failed at the same rate as six with it
  enabled — see [docs/measurements.md](docs/measurements.md#isolating-the-restore-panic).
  And it tracks *host* load rather than migration count: on a laptop swapping
  hard (load 6+, 12% free RAM) it hit 5 of 6 migrations, while the same build
  on an unloaded host migrates cleanly. That is consistent with aarch64
  timer/GIC state after a restore the host failed to schedule promptly. The
  failure is safe rather than lossy: the migration returns an error, rollback
  leaves the source authoritative, and the guest keeps serving — and `make
  bench` reclaims the slot (`DELETE /vms/{id}`) and reboots a fresh guest so
  the distribution still completes.
- **Post-copy tail** (serve residual pages over the network for a near-zero
  final round) is designed-for but not implemented — the natural next step.

## AI usage

This project was built pair-programming with an AI agent (Claude); the
division of labor, what it got wrong, and what I had to fix are documented in
[docs/ai-usage.md](docs/ai-usage.md).
