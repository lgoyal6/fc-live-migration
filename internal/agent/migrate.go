//go:build linux

package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/lgoyal6/fc-live-migration/internal/fcapi"
	"github.com/lgoyal6/fc-live-migration/internal/sparse"
	"github.com/lgoyal6/fc-live-migration/internal/timeline"
)

// Migration tuning. Values chosen for a laptop-hosted demo under nested
// virtualization; see docs/architecture.md for how they trade off against
// each other. On bare metal, larger budgets and shorter breathers converge
// faster with less brownout.
const (
	// convergedBytes: stop pre-copying when a round's dirty set is at most
	// this large — the final paused round then has almost nothing to do.
	// Firecracker re-marks virtio queue pages dirty after every snapshot,
	// so rounds never shrink below a few hundred KiB; the plateau check
	// below handles that floor.
	convergedBytes = 1 << 20
	// plateauFactor: when a round fails to shrink the dirty set below this
	// fraction of the previous one, pre-copy has converged as far as the
	// workload allows. Two plateaued rounds in a row while already small
	// (< plateauBytes) → go final; while large → escalate the throttle.
	plateauFactor = 0.9
	plateauBytes  = 4 << 20
	// maxRounds bounds pre-copy; combined with throttling this guarantees
	// termination even for a guest that dirties memory adversarially fast.
	// Budgeted live drains take many small rounds by design, so the cap is
	// sized in budget units (256 × 2MiB ≈ 2× guest memory for the demo VM).
	maxRounds      = 256
	maxRoundsStock = 8
	// drainBudgetBytes caps one DiffLive call's dump. The dump runs on the
	// VMM event-loop thread, which also services virtio, so this cap bounds
	// how long guest I/O stalls per round. Bigger budget = fewer rounds (less
	// per-round overhead, snappier drain) but a longer stall per round; the
	// total brownout duration is ~constant either way (it is set by the dirty
	// set and the drain rate, not the chunk size). 2 MiB is a good balance on
	// nested virt, where each dump costs several ms.
	drainBudgetBytes = 2 << 20
	// throttleEscalateEvery: once the drain has run longer than the largest
	// honest backlog could take (the guest's whole memory at our transfer
	// pace, ×2 for slack), the guest is provably re-dirtying faster than we
	// ship — escalate auto-converge one level per interval from there.
	// Escalating any earlier punishes ordinary large backlogs that are
	// converging on their own; an early version of this code did exactly
	// that and duty-cycle-froze a perfectly drainable guest.
	throttleEscalateEvery = 2 * time.Second
	// roundBreather: idle time between drain rounds during which the VMM
	// event loop services virtio uninterrupted, so the guest answers
	// traffic. Larger breather → lower brownout severity, slower drain.
	roundBreather = 12 * time.Millisecond
	// settleBeforePause: after the drain converges, stop dumping briefly so
	// the guest returns to full-speed service before the final pause — a
	// clean boundary for the client-observed switchover. Kept short on
	// purpose: whatever the guest re-dirties during the settle lands in the
	// final (in-blackout) round, so a long settle would inflate the very
	// blackout it is meant to isolate. A few tens of ms is enough for the
	// guest to answer a handful of probes.
	settleBeforePause = 40 * time.Millisecond
	// Pacing for transfers that run while the guest serves traffic over the
	// same links: an aggressive stream congests the shared software bridge
	// (softirq contention + queueing) and the guest's own packets pay for
	// it. The base image gets more bandwidth (one long transfer, tolerable
	// degradation); drain rounds run gentler because they repeat for the
	// whole convergence phase. Only the final (blackout) transfer is
	// unpaced — pacing there would be exactly wrong.
	baseTransferMBps  = 120
	roundTransferMBps = 60
)

// RoundStat reports one pre-copy round in the migration report.
type RoundStat struct {
	Round      int   `json:"round"`
	DirtyBytes int64 `json:"dirty_bytes"`
	StartNS    int64 `json:"start_ns"`       // CLOCK_MONOTONIC, aligns with probe gaps
	SnapshotNS int64 `json:"snapshot_ns"`    // the DiffLive API call (event-loop stall)
	TransferNS int64 `json:"transfer_ns"`    // extent walk + wire + target splat
	Paused     bool  `json:"paused"`         // true = stock Diff (fallback mode)
	Throttle   int   `json:"throttle_level"` // auto-converge level in effect
}

// Report is the source agent's account of one migration.
type Report struct {
	VM         string           `json:"vm"`
	Target     string           `json:"target"`
	LiveRounds bool             `json:"live_rounds"` // DiffLive patch in use
	BaseBytes  int64            `json:"base_bytes"`
	Rounds     []RoundStat      `json:"rounds"`
	FinalBytes int64            `json:"final_bytes"`
	BlackoutNS int64            `json:"blackout_ns"`
	Timeline   []timeline.Event `json:"timeline"`
}

// migrate drives the whole source side of a migration. On success the VM is
// running on the target and the local process is gone. On failure the local
// VM is resumed and keeps running (migration is all-or-nothing).
func (a *Agent) migrate(ctx context.Context, vm *VM, target string) (*Report, error) {
	vm.mu.Lock()
	if vm.state != StateRunning {
		vm.mu.Unlock()
		return nil, fmt.Errorf("vm %s is %s, not running", vm.Cfg.ID, vm.state)
	}
	vm.state = StateMigrating
	vm.mu.Unlock()

	rec := &timeline.Recorder{}
	rec.Mark("migrate.start")
	a.events.publish("migrate.start", vm.Cfg.ID)

	rep, err := a.migrateInner(ctx, vm, target, rec)
	if err != nil {
		// Roll back: the source VM must survive a failed migration. The
		// dirty-page state consumed by completed rounds now exists only in
		// the target's discarded image, so force a fresh base next time.
		a.events.publish("migrate.abort", fmt.Sprintf("%s: %v", vm.Cfg.ID, err))
		_ = a.postJSON(ctx, target, "/internal/vms/"+vm.Cfg.ID+"/abort", nil, nil)
		vm.needsResync = true
		if rerr := vm.api.Resume(); rerr == nil {
			vm.setState(StateRunning)
		} else {
			vm.setState(StateDead)
			return nil, fmt.Errorf("migration failed (%w) and source resume also failed: %v", err, rerr)
		}
		return nil, err
	}

	vm.kill()
	vm.setState(StateMigrated)
	a.events.publish("migrate.done",
		fmt.Sprintf("%s blackout=%.2fms", vm.Cfg.ID, float64(rep.BlackoutNS)/1e6))
	return rep, nil
}

func (a *Agent) migrateInner(ctx context.Context, vm *VM, target string, rec *timeline.Recorder) (*Report, error) {
	rep := &Report{VM: vm.Cfg.ID, Target: target, LiveRounds: a.cfg.LiveRounds}

	// 1. Target spawns a Firecracker process now, so none of that cost lands
	// inside the blackout window.
	if err := a.postJSON(ctx, target, "/internal/vms/"+vm.Cfg.ID+"/prepare", vm.Cfg, nil); err != nil {
		return nil, fmt.Errorf("prepare target: %w", err)
	}
	rec.Mark("target.prepared")

	// 2. Re-establish a fresh full base if a previous abort invalidated ours.
	if vm.needsResync {
		if err := vm.api.Pause(); err != nil {
			return nil, err
		}
		err := vm.api.CreateSnapshot(fcapi.SnapshotFull, vm.baseStatePath(), vm.baseMemPath(), 0)
		if rerr := vm.api.Resume(); rerr != nil {
			return nil, rerr
		}
		if err != nil {
			return nil, err
		}
		vm.baseMem = vm.baseMemPath()
		vm.needsResync = false
		rec.Mark("base.resynced")
	}

	// 3. Ship the base image while the guest keeps running, paced so the
	// stream doesn't starve the guest's own traffic.
	n, err := a.sendMemFile(ctx, target, vm.Cfg.ID, "base", vm.baseMem, baseTransferMBps)
	if err != nil {
		return nil, fmt.Errorf("send base image: %w", err)
	}
	rep.BaseBytes = n
	rec.Mark("base.sent")

	// 4. Pre-copy rounds: copy what the guest dirtied while we were copying.
	// With the DiffLive patch the guest never stops during these; in stock
	// fallback mode each round costs a short pause (and counts against the
	// blackout budget the client observes).
	// Budgeted live rounds bound the guest-visible stall of any single call;
	// stock (paused) rounds don't support budgets, so they stay whole.
	budget := int64(0)
	rounds := maxRoundsStock
	if a.cfg.LiveRounds {
		budget = drainBudgetBytes
		rounds = maxRounds
	}

	throttleLevel := 0
	prevDirty := int64(-1)
	plateaued := 0
	drainStart := time.Now()
	// An honest backlog is at most the guest's whole memory; give it 2× at
	// our pace before concluding the guest outruns the drain.
	honestDrain := 2 * time.Duration(vm.Cfg.MemMiB) * time.Second / roundTransferMBps
	nextEscalation := drainStart.Add(honestDrain)
	for r := 1; r <= rounds; r++ {
		roundStart := timeline.Now()
		memPath := vm.roundMemPath(r)

		dirty, err := a.captureRound(vm, memPath, budget)
		if err != nil {
			return nil, fmt.Errorf("pre-copy round %d: %w", r, err)
		}
		snapshotDone := timeline.Now()
		// Rounds run while the guest serves traffic, so they get paced;
		// only the final (blackout) transfer is unpaced.
		if _, err := a.sendMemFile(ctx, target, vm.Cfg.ID, fmt.Sprintf("%d", r), memPath, roundTransferMBps); err != nil {
			return nil, fmt.Errorf("send round %d: %w", r, err)
		}
		_ = os.Remove(memPath)

		rep.Rounds = append(rep.Rounds, RoundStat{
			Round: r, DirtyBytes: dirty, StartNS: roundStart,
			SnapshotNS: snapshotDone - roundStart,
			TransferNS: timeline.Now() - snapshotDone,
			Paused:     !a.cfg.LiveRounds, Throttle: throttleLevel,
		})
		a.events.publish("round", fmt.Sprintf("%s r=%d dirty=%dKiB throttle=%d",
			vm.Cfg.ID, r, dirty>>10, throttleLevel))

		// A full budget means the dirty backlog isn't drained yet — keep
		// going, escalating the throttle if the guest outruns us for long.
		if budget > 0 && dirty >= budget {
			if time.Now().After(nextEscalation) && throttleLevel < maxThrottleLevel {
				throttleLevel++
				nextEscalation = time.Now().Add(throttleEscalateEvery)
				a.events.publish("throttle.engage",
					fmt.Sprintf("%s level=%d after %.1fs of non-converging drain",
						vm.Cfg.ID, throttleLevel, time.Since(drainStart).Seconds()))
				if err := a.throttle.SetLevel(vm.Cfg.ID, throttleLevel); err != nil {
					a.events.publish("throttle.error", err.Error())
				}
			}
			time.Sleep(roundBreather)
			continue
		}

		if dirty <= convergedBytes {
			break
		}
		if prevDirty > 0 && float64(dirty) > plateauFactor*float64(prevDirty) {
			plateaued++
		} else {
			plateaued = 0
		}
		// A small, stable dirty set is as converged as this workload gets.
		if plateaued >= 2 && dirty <= plateauBytes {
			break
		}
		// A large, stable dirty set means the guest writes faster than we
		// copy — auto-converge by slowing its CPU until rounds shrink.
		if plateaued >= 1 && dirty > plateauBytes && throttleLevel < maxThrottleLevel {
			throttleLevel++
			if err := a.throttle.SetLevel(vm.Cfg.ID, throttleLevel); err != nil {
				a.events.publish("throttle.error", err.Error())
			}
		}
		prevDirty = dirty
	}

	// Release any auto-converge throttle and let the guest run at full speed
	// briefly, so it is fully responsive right up to the pause. The residual
	// this settle re-dirties is tiny and gets swept into the final round.
	defer func() { _ = a.throttle.SetLevel(vm.Cfg.ID, 0) }()
	if err := a.throttle.SetLevel(vm.Cfg.ID, 0); err != nil {
		return nil, fmt.Errorf("release throttle: %w", err)
	}
	time.Sleep(settleBeforePause)

	// 5. The blackout window: pause, capture the residual dirty set plus the
	// vCPU/device state, ship both, restore-and-resume on the target, flip
	// the network. Everything in here is proportional to the residual dirty
	// set, never to guest memory size.
	blackoutStart := rec.Mark("blackout.pause")
	if err := vm.api.Pause(); err != nil {
		return nil, err
	}
	if err := vm.api.CreateSnapshot(fcapi.SnapshotDiff, vm.finalStatePath(), vm.finalMemPath(), 0); err != nil {
		return nil, err
	}
	rec.Mark("blackout.snapshotted")

	n, err = a.sendMemFile(ctx, target, vm.Cfg.ID, "final", vm.finalMemPath(), 0)
	if err != nil {
		return nil, fmt.Errorf("send final round: %w", err)
	}
	rep.FinalBytes = n
	if err := a.sendFile(ctx, target, "/internal/vms/"+vm.Cfg.ID+"/vmstate", vm.finalStatePath()); err != nil {
		return nil, fmt.Errorf("send vmstate: %w", err)
	}
	rec.Mark("blackout.state_sent")

	var fin finalizeResponse
	if err := a.postJSON(ctx, target, "/internal/vms/"+vm.Cfg.ID+"/finalize", nil, &fin); err != nil {
		return nil, fmt.Errorf("finalize on target: %w", err)
	}
	// Target timestamps share our CLOCK_MONOTONIC (same kernel); merge them.
	for _, e := range fin.Timeline {
		rec.MarkAt(e.Name, e.NS)
	}
	rep.BlackoutNS = fin.ResumedNS - blackoutStart
	rep.Timeline = rec.Events()
	return rep, nil
}

// captureRound takes one pre-copy memory snapshot. Live mode uses the
// patched DiffLive type (vCPUs keep running, dump capped at budget bytes);
// fallback mode brackets a stock Diff with pause/resume.
func (a *Agent) captureRound(vm *VM, memPath string, budget int64) (int64, error) {
	if a.cfg.LiveRounds {
		if err := vm.api.CreateSnapshot(fcapi.SnapshotDiffLive, memPath+".unused-state", memPath, budget); err != nil {
			return 0, err
		}
	} else {
		if err := vm.api.Pause(); err != nil {
			return 0, err
		}
		err := vm.api.CreateSnapshot(fcapi.SnapshotDiff, memPath+".state", memPath, 0)
		if rerr := vm.api.Resume(); rerr != nil {
			return 0, rerr
		}
		if err != nil {
			return 0, err
		}
		_ = os.Remove(memPath + ".state")
	}
	f, err := os.Open(memPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	extents, err := sparse.WalkExtents(f)
	if err != nil {
		return 0, err
	}
	return sparse.TotalBytes(extents), nil
}

// finalizeResponse is what the target returns from /finalize.
type finalizeResponse struct {
	ResumedNS int64            `json:"resumed_ns"`
	Timeline  []timeline.Event `json:"timeline"`
}

// sendMemFile streams a (sparse) memory file to the target agent, pacing to
// paceMBps if non-zero.
func (a *Agent) sendMemFile(ctx context.Context, target, vmID, round, path string, paceMBps int) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	pr, pw := io.Pipe()
	var sent int64
	go func() {
		// Buffering matters: a diff of randomly-dirtied pages produces one
		// tiny wire record per page, and pushing each through the pipe (and
		// then the HTTP stack) individually caps throughput at ~140MB/s.
		bw := bufio.NewWriterSize(pw, 256<<10)
		var w io.Writer = bw
		if paceMBps > 0 {
			w = &pacedWriter{w: bw, bytesPerSec: int64(paceMBps) << 20, start: time.Now()}
		}
		n, err := sparse.Send(w, f)
		if err == nil {
			err = bw.Flush()
		}
		sent = n
		pw.CloseWithError(err)
	}()

	url := fmt.Sprintf("http://%s/internal/vms/%s/mem?round=%s", target, vmID, round)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, pr)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := a.peerClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return 0, fmt.Errorf("target rejected %s round %s: HTTP %d: %s", vmID, round, resp.StatusCode, body)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return sent, nil
}

// sendFile POSTs a small file (the vmstate) verbatim.
func (a *Agent) sendFile(ctx context.Context, target, path, filePath string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+target+path, f)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := a.peerClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("target rejected %s: HTTP %d: %s", path, resp.StatusCode, body)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// postJSON POSTs a JSON body to the target agent and decodes the response.
func (a *Agent) postJSON(ctx context.Context, target, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+target+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.peerClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("%s: HTTP %d: %s", path, resp.StatusCode, b)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// pacedWriter caps throughput by sleeping whenever writes run ahead of the
// target rate — the "migration bandwidth limit" every serious hypervisor
// exposes, so background transfers never starve the guest's own traffic.
type pacedWriter struct {
	w           io.Writer
	bytesPerSec int64
	start       time.Time
	written     int64
}

func (p *pacedWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.written += int64(n)
	ahead := time.Duration(p.written)*time.Second/time.Duration(p.bytesPerSec) - time.Since(p.start)
	if ahead > 0 {
		time.Sleep(ahead)
	}
	return n, err
}

// peerHTTPClient builds the client used for agent→agent transfers. Keep-alive
// matters: the final round must reuse a warm TCP connection, not pay a
// handshake inside the blackout window.
func peerHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     5 * time.Minute,
		},
		Timeout: 10 * time.Minute, // base-image streams can be large
	}
}
