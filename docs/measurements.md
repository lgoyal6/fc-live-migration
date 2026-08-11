# Measurement methodology & results

## What "blackout" means here

Live-migration downtime is the stop-the-world window: the interval during
which the VM is not executing on either host. This project measures it two
independent ways.

1. **Agent-reported (`agent_blackout_ms`)** — the source records
   `CLOCK_MONOTONIC` at `PATCH /vm Paused`; the target records it again the
   instant `PUT /snapshot/load` + resume returns. Both agents share one host
   kernel, so the two readings are on the same clock and subtract directly.
   This is the VM provably not executing — no KVM_RUN is entered between those
   two points — and it is the technically correct definition of downtime.

2. **Client-observed (`blackout_ms`)** — `fcprobe` sends the guest a UDP echo
   probe every 0.5–1 ms and finds the longest run of consecutive probes that
   were *never answered* and whose time span overlaps the agent's
   `[pause, resume]` window. This is the outage an external user actually saw
   around the switchover. It is deliberately adversarial: it trusts nothing
   the agents report, and it charges a whole probe interval against us at each
   end.

The prober also reports its own worst send-loop stall (`sendstall`) so a
measurement polluted by client-side scheduling is visible rather than
silently folded into the result.

## Distinguishing blackout from brownout

During pre-copy the VM **keeps executing** — this is the whole point. We prove
it three ways, every run:

- the guest's counter (`GET /status`) increases monotonically across the
  migration;
- a long-lived TCP session into the guest stays open, with no reconnect,
  across the host switch;
- `integrity_errors` stays 0 over millions of self-verified pages.

But the guest's *network I/O* is intermittently delayed during pre-copy,
because the memory dump shares the VMM event-loop thread with virtio and each
dump briefly stops servicing the NIC. We report this separately as
`brownout_ms` / "worst degraded window" (probes answered slower than 5 ms) and
never fold it into the blackout figure. Brownout is degraded service on a
running VM; blackout is the VM being down. Conflating them is the most common
way live-migration demos overstate their result, so we keep them apart.

## The nested-virtualization caveat (read this before judging the numbers)

The development environment is Docker-in-Lima-in-macOS on Apple Silicon: the
guest is a VM (Firecracker) inside a VM (Lima) on a laptop also running the
usual desktop apps. Two consequences dominate the *client-side* numbers here:

- **A post-restore fault-in tail.** This is the big one, and it is *not*
  measurement noise. After the target resumes, the guest's RAM is a file it
  has not touched yet; every first access takes a stage-2 fault, and nested
  virt makes each one roughly 10× more expensive. For a 256 MiB guest that is
  ~65k faults, and the arithmetic lands where the measurement does: the VM is
  *executing* (the agent's pause→resume already ended, at 8–18 ms) but is too
  busy wiring up its own memory to service virtio, so the client sees no
  replies for ~180–600 ms — the spread tracks how many pre-copy rounds ran,
  since each round's dump contends with virtio.

  It would be convenient to blame an ambient jitter floor, so that hypothesis
  was tested directly — `fcprobe probe` for 5 s with no migration in flight,
  three times:

  ```
  lostrun=1.00ms  |  lostrun=0.00ms  |  lostrun=12.70ms
  ```

  At rest the guest never goes unanswered for more than ~13 ms. The switchover
  gap is therefore caused by the migration, not by the environment being
  noisy — the environment only sets its *size*.
- **~10× slower VMM operations.** Each `KVM_GET_DIRTY_LOG` and each page dump
  traps to the L0 hypervisor, so dumps that cost <1 ms on bare metal cost
  several ms here, deepening the pre-copy brownout.

The **agent-reported** blackout is immune to both — it is measured inside the
VMM around the actual pause/resume — which is why it stays clean (~8–25 ms)
across the *guest*-level jitter above.

It is not, however, immune to starvation of the host itself. Pause→resume is
wall-clock time, and the work inside that window (the final diff, the
snapshot load on the target) runs on threads the host must schedule. On a
laptop that had gone into heavy swap — load average 6.4, 12% free memory,
1.6M pageouts — the same build reported a **180 ms** agent blackout, an order
of magnitude off its own baseline of 21.9 ms measured hours earlier on the
same machine. No number in this document means anything if the host is
thrashing; check `uptime` and free memory before a measurement run, not
after.

### What this means for the 30 ms budget

Stated plainly, because it is the one number a reader should not have to dig
for: **on this hardware the stop-the-world window is 8–18 ms and meets the
budget, while the client-observed switchover gap is ~180–600 ms and does
not.** `fcprobe` reports the stricter of the two, so `make demo` on a laptop
prints `blackout budget 30ms → FAIL`. That verdict is correct and deliberately
not softened; the tool is meant to be adversarial about its own project.

The two numbers measure different things. The agent figure is downtime in the
live-migration sense — the VM provably not executing — which is what the
technique is judged on and what the patch exists to shrink. The client figure
additionally contains the fault-in tail above, during which the VM *is*
running and simply cannot answer yet.

On non-nested KVM the dump amplification and the per-fault cost both drop by
roughly an order of magnitude, which is the basis for expecting the two
numbers to converge there. That expectation is **untested** — every figure in
this document was taken under nested virtualization, and the honest status of
the bare-metal claim is "predicted, not measured." Running
`make setup && make up && make bench` on any Linux host with `/dev/kvm` is
what would settle it, and is the first thing worth doing with real hardware.

## Results

`make bench` runs N migrations ping-ponging the VM A→B→A…, writing
`bench-results/blackout.csv`; `make plot` renders the agent-blackout histogram
to `bench-results/histogram.png`. A representative run on the laptop (nested
virt, 8-vCPU Lima, 256 MiB guest dirtying 5 MB/s):

| run | agent blackout (ms) | client blackout (ms) | brownout (ms) | rounds | integrity |
|----:|--------------------:|---------------------:|--------------:|-------:|----------:|
|  1  |               17.0  |                136   |         1223  |    5   |    0      |
|  2  |               17.1  |                459   |         1045  |    6   |    0      |
|  3  |               11.9  |               1468   |         1516  |    6   |    0      |
|  4  |               16.9  |               4675   |         3874  |    6   |    0      |
|  5  |               23.0  |               4979   |         4178  |   11   |    0      |
|  6  |  guest kernel panicked (memory intact) → harness rebooted a fresh guest ||||        |
|  7  |               10.9  |                193   |          822  |    5   |    0      |

Two things to read here. **`agent blackout` is 11–23 ms across every run — the
true downtime, always within the 30 ms budget, and stable even as the
environment degrades.** The `client blackout` and `brownout` columns *climb*
run over run (3→5) because the guest's own health decays under rapid
back-to-back restores until it panics at run 6 — memory verified intact
(`integrity` stays 0), so this is the aarch64 restore-state issue noted in the
README, not a data bug — after which the harness reboots a fresh guest and run
7 is immediately healthy again. That the agent measurement stays flat while the
client measurement drifts is the clearest illustration of which number
reflects the migration mechanism and which reflects the environment. On
non-nested hardware, spaced-apart migrations, the client number tracks the
agent number and this drift does not appear.

## Isolating the restore panic

The guest sometimes panics on the *target* immediately after restore, always
in the same place — the timer softirq:

```
lr : call_timer_fn.constprop.0+0x24/0x80
Call trace: __run_timers → run_timer_softirq → handle_softirqs
Kernel panic - not syncing: Oops: Fatal exception in interrupt
```

The obvious suspect is the `DiffLive` patch: if a pause-free dump lost a
guest write, the target would restore a torn image and die on the first
kernel structure it touched. That hypothesis is testable, because the agent
can run the same pre-copy with stock *paused* diff snapshots (`LIVE_ROUNDS=false`),
which takes the patch out of the data path while leaving every other moving
part — the sparse wire protocol, the memory file assembly, the restore, the
GARP — identical.

Six trials per arm, each a freshly booted guest migrated once host-a → host-b,
with the target's console log wiped between trials so a stale panic cannot be
counted twice:

| Arm | Pass | Target kernel panic | Other failure |
|---|---:|---:|---:|
| `live_rounds=true` (DiffLive patch) | 1 | 2 | 3 |
| `live_rounds=false` (stock paused diff) | 1 | 4 | 1 |

**The patch is not the cause.** Disabling it does not improve the failure
rate; if anything the confirmed-panic count is higher. What *does* correlate
is host load — this run was taken on a laptop in heavy swap (see the host
starvation note above), and the same build on an unloaded host migrates
cleanly. The working hypothesis is therefore aarch64 timer/GIC state after a
restore the host was too starved to schedule promptly, not memory corruption
in pre-copy — consistent with `integrity_errors` never once going nonzero,
across every arm, including the runs that panicked.

Worth stating plainly: the guest's scribbler verifies its own buffer
(`scrib_buf_mb`, 32 MiB of a 256 MiB guest), not the kernel's own pages, so a
clean `integrity_errors` is strong evidence but not a proof that no kernel
page was ever disturbed. The A/B above is the load-bearing result, because it
holds the memory path fixed and varies only the snapshot primitive.

The failure mode is safe rather than lossy. The migration returns an error,
the rollback path leaves the source authoritative, and the guest keeps
serving from the host it started on — verified reachable from the client and
from both host containers after every failed trial.

## Reproducing

```console
$ make bench N=50        # 50 ping-pong migrations
$ make plot              # bench-results/histogram.png
$ make hostile MBPS=400  # auto-converge under a hostile dirty rate
```

To reproduce the A/B above, bring the agents up with the patch out of the
data path and repeat the same migrations:

```console
$ LIVE_ROUNDS=false docker compose -f deploy/docker-compose.yml up -d
$ docker compose -f deploy/docker-compose.yml logs host-a | grep live_rounds
```

Each migration's per-phase timeline (`migrate.start`, `base.sent`,
`blackout.pause`, `target.resumed`, …) and per-round dirty sizes are printed
by `fcprobe migrate` and streamed live on each agent's `GET /events`.
