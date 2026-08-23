# Architecture

## The problem, precisely

A running microVM is state in four places: guest RAM, vCPU registers, KVM
in-kernel state (interrupt controller, timers), and emulated device state
(virtio queues). Live migration moves all of it to another host while the
guest keeps running, so a user of the guest sees at most a brief blip.

Firecracker gives you the pieces to *stop* and move a VM - `PATCH /vm`
(pause), `PUT /snapshot/create`, `PUT /snapshot/load` - but not to move it
live. The specific obstruction: **every snapshot requires paused vCPUs.** Not
by an explicit check in the snapshot API, but because the vCPU state save
(`save_state` → `VcpuEvent::SaveState`) answers `NotAllowed` unless the vCPU
is in its paused state (`src/vmm/src/vstate/vcpu.rs`). So the textbook
pre-copy algorithm - copy RAM while the guest runs, iterate on the pages it
dirties, then stop only for a tiny final round - isn't expressible: each
"copy RAM" step would have to pause first, and the pauses are the very thing
pre-copy exists to avoid.

## The patch: `SnapshotType::DiffLive`

The key observation (verified by reading the v1.16.1 source) is that the
*memory dump* has no paused requirement - only the *vCPU/device state save*
does. `Vm::snapshot_memory_to_file` takes `&self`, reads guest memory through
`VolatileSlice`, and gets the dirty set from `KVM_GET_DIRTY_LOG`, which is
explicitly designed to be called on a running VM. Only `create_snapshot`'s
call to `save_state` (before the memory dump) needs the pause.

`patches/0001-*.patch` adds a snapshot type that dumps dirty guest memory
while the vCPUs run and **skips the state save entirely**:

```
create_snapshot(Diff)      = save_state + state file + dump dirty memory   (needs pause)
create_snapshot(DiffLive)  =                            dump dirty memory   (no pause)
```

Correctness has two halves:

- **Guest (vCPU) writes racing the dump.** KVM's dirty log is fetch-and-clear.
  A write that lands while we dump is either already in the bitmap we fetched
  (its page gets dumped, possibly torn mid-write) or in KVM's freshly-cleared
  bitmap (so it's re-reported next round). Either way the page is marked dirty
  again, so a torn page is always re-copied by a later round, and the final
  paused round is consistent. Torn pages are harmless precisely because they
  are never the *last* copy of that page.
- **VMM-side writes (virtio RX, MMDS).** These run on the same event-loop
  thread as the dump, so they cannot execute *during* it - no race to worry
  about. But upstream's `dump_dirty` resets the VMM-internal dirty bitmap on
  success; doing that mid-migration would drop VMM writes that happened
  between the dump and a later round. So `dump_dirty_live` **preserves** that
  bitmap (subject to the budget below), and only the final paused `Diff`
  resets it.

### Bounding the event-loop stall

The dump runs on the event loop that also services virtio, so a large dump
starves guest I/O. `DiffLive` takes a `live_max_bytes` budget: it dumps at
most that many bytes, records the over-budget dirty pages back into the VMM
bitmap, and returns. That bitmap is the **drain backlog** - the next call
fetches the KVM log again (picking up new guest writes), ORs in the backlog,
dumps another budget's worth, and so on. Because the guest dirties less than
a budget per round-cycle in the common case, the residual shrinks each round
and the drain converges. The unit tests
(`vstate::memory::tests::test_dump_dirty_live_*`) pin the drain, the budget
split, and the backlog accounting.

The patch is ~250 lines across six files; it changes nothing about `Full` or
`Diff`, adds a swagger enum value, and passes the upstream snapshot test
suite unchanged.

## Control plane: `migrated`

One agent per host owns the local Firecracker processes and speaks a small
agent↔agent protocol. A migration, driven by the source:

1. **prepare** - the target spawns a Firecracker process and opens its API
   socket. Off the critical path, so its cost never touches the blackout.
2. **base image** - the source streams a full memory image (the
   provision-time snapshot, or the image a previous migration left) to the
   target while the guest runs. Paced, over the dedicated migration network.
3. **pre-copy drain** - rounds of `DiffLive`, each dumping the budget and
   streaming only the dirty extents (`SEEK_DATA`/`SEEK_HOLE`, so the wire cost
   is the dirty set, not RAM size). Loops until the residual is small or the
   guest is provably out-dirtying the drain (→ auto-converge).
4. **settle** - stop dumping briefly so the guest returns to full-speed
   service, giving a clean boundary before the pause.
5. **blackout** - `PATCH /vm Paused` → final `Diff` (residual dirty +
   vCPU/device state) → stream both → `PUT /snapshot/load` + resume on the
   target → gratuitous ARP. Then kill the source VM.

Every phase is timestamped against `CLOCK_MONOTONIC` and streamed over SSE.
Failure at any step rolls back: the source resumes and stays authoritative,
and the next migration re-establishes a fresh base.

### Why the transfers are on a second network

The base image is hundreds of MB. If it shares the guest's L2, the transfer
contends with the guest's own packets and the client sees a brownout even
though the VM is up. Production clusters put live migration on a dedicated
network for exactly this reason; the compose file gives the hosts a second
interface (`xfernet`, 172.31.0.0/24) that carries all agent↔agent traffic,
leaving the guest L2 (`mignet`, 172.30.0.0/24) clear.

## Network identity follows the VM

The guest holds its own MAC+IP directly on `mignet`: inside each host
container, `br0` bridges the guest's `tap0` with the host's `mignet` uplink
(`deploy/entrypoint.sh`). Because the MAC and IP are baked into the snapshot,
the guest keeps them on the target. On resume, the target agent broadcasts a
**gratuitous ARP** with the guest's source MAC (`internal/agent/garp.go`, raw
`AF_PACKET`); every bridge on the path moves the guest's forwarding entry to
the new port, and in-flight TCP connections simply start arriving there. No
proxy, no reconnect - the vMotion trick.

## Auto-converge

If the guest dirties memory faster than the drain ships it, pre-copy never
converges. Once the drain has run longer than the guest's whole memory would
take at the transfer rate, the agent concludes the guest is out-running it and
duty-cycles `SIGSTOP`/`SIGCONT` on the Firecracker **vCPU threads only**
(`internal/agent/throttle.go`, found by their `fc_vcpu` thread names). The VMM
event loop and the drain keep running at full speed - the guest slows, the
dirty rate drops, the drain catches up. This is QEMU's vCPU-throttling
auto-converge, implemented from the control plane instead of inside the VMM.
`make hostile` demonstrates it against a guest scribbling memory at 400 MB/s.

## Correctness instrumentation

The guest (`cmd/guestd`) continuously rewrites a buffer of self-describing
pages - each page's bytes are a deterministic keystream seeded by its own
index and iteration counter - and verifies a sample every iteration. Any page
that is torn, stale, or lost anywhere in the migration pipeline fails
verification and increments `integrity_errors`, which the prober reads after
every migration. Across every run in this project that counter has stayed
zero, which is the empirical evidence that the torn-page reasoning above holds
in practice.
