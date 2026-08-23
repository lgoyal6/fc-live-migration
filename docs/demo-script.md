# 120-second video script

Goal: show a running Firecracker microVM live-migrate between two hosts with a
blackout well under 30 ms, without dropping its TCP session, and explain the
one idea that makes it possible. Keep it tight - the numbers do the talking.

Record on a **bare-metal Linux box** (`/dev/kvm`, not nested) so the
client-observed blackout is clean; see docs/measurements.md for why the
laptop's nested-virt jitter muddies client-side numbers.

## Setup before recording

```
make vm        # (skip on the bare-metal box)
make setup     # build Firecracker + patch, kernel, rootfs  (do this off-camera)
make up
```

Three tmux panes: **[client]** dashboard, **[host-a]** `migrated` events,
**[host-b]** `migrated` events. In host-a/host-b panes:
`docker compose -f deploy/docker-compose.yml logs -f host-a` (and host-b).

## Beat sheet (~120s)

**0:00–0:12 - The hook.** "Firecracker's maintainers say it doesn't do live
migration. So I added it to the VMM." Show the one-line patch summary:
`git --no-pager show --stat` on the patch, or just the `SnapshotType::DiffLive`
enum on screen for a beat.

**0:12–0:30 - The setup.** "Two hosts, Docker containers, each with its own
Firecracker and KVM. A guest with a fixed IP lives on the shared L2." Boot it:
```
make demo        # or the curl POST, so viewers see the VM come up
```
Show `GET /status`: counter ticking, `integrity_errors: 0`.

**0:30–0:45 - The live proof.** Start a TCP session into the guest that you'll
keep on screen the whole time:
```
docker compose -f deploy/docker-compose.yml exec client \
  sh -c 'while true; do curl -s http://172.30.0.50:7780/status; sleep 0.2; done'
```
Point at the monotonically increasing counter. "This never stops or resets
during the migration."

**0:45–1:05 - Migrate.** Trigger it (the `make demo` migration, or
`fcprobe migrate`). Narrate the phases as the agent events scroll: base image
streams over the dedicated migration network, pre-copy rounds shrink the dirty
set, then one short pause, restore on host-b, gratuitous ARP. The counter
stream on the left never breaks.

**1:05–1:20 - The number, and the asterisk.** Freeze on fcprobe's output:
```
agent  pause→resume    :  7.9-17.7 ms  (VM provably not executing)
client bracketing probe:  179-324 ms   (fault-in tail — see below)
guest integrity        :  0 errors
blackout budget 30ms → FAIL
```
"Eight milliseconds of blackout - the VM provably not executing. TCP session
survived, zero corrupted pages."

Then take the `FAIL` head-on, in one breath - do not let a viewer find it
first: "My own prober fails me on the client-side number, and that's honest.
That 200 ms isn't the pause - the VM is already running. It's the guest
faulting 256 MB of RAM back in, and every one of those 65,000 faults costs
about ten times what it would on real hardware, because this is a VM inside a
VM on a laptop. I probed it idle to rule out noise: 13 ms. On bare metal that
tail should collapse - I didn't have bare metal, so I'm not claiming it."

**1:20–1:40 - Why it works, in one sentence.** "The trick: dump dirty memory
while the vCPUs keep running - a snapshot type Firecracker didn't have - so
only the last tiny round needs a pause." Show the pre-copy → single-pause
timeline (the phase list, or the histogram from `make plot`).

**1:40–2:00 - The flex + honesty.** Either the histogram of N runs (all under
budget) or `make hostile` converging under a 400 MB/s dirty rate via
auto-converge. Close: "Runs on a laptop with nested virt or a real box - one
`make demo`." One line on what's next (post-copy tail) if time allows.

## Do / don't

- DO keep the TCP counter visible the whole migration - it's the "no
  interruption" proof.
- DO show `integrity_errors: 0` - correctness, not just speed.
- DON'T claim the client number on the laptop; use the agent number there and
  say why, or record on bare metal.
- DON'T hide the brownout if it's visible - name it, and note it's degraded
  service on a running VM, not downtime.
