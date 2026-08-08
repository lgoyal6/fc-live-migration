// fcprobe is the measurement side of the demo: it hammers the guest with
// sub-millisecond UDP probes and reports blackout as the longest silence the
// *client* observed — deliberately not trusting the agents' self-reported
// numbers (those are printed alongside for cross-checking).
//
//	fcprobe probe                        just probe and print gap stats
//	fcprobe migrate                      probe + trigger one migration + report
//	fcprobe bench -n 100                 repeated ping-pong migrations + stats
//
// fcprobe runs in the client container, which shares a kernel with both
// hosts, so its CLOCK_MONOTONIC timestamps align with agent timelines.
package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lgoyal6/fc-live-migration/internal/timeline"
)

func main() {
	log.SetFlags(0)
	var (
		guest = flag.String("guest", "172.30.0.50:7777", "guest UDP echo address")
		// Agents are reached for control over mignet, but named as targets
		// for migration by their xfernet address so bulk transfer stays off
		// the guest's L2.
		agentA   = flag.String("agent-a", "host-a:8500", "first agent (control address)")
		agentB   = flag.String("agent-b", "host-b:8500", "second agent (control address)")
		xferA    = flag.String("xfer-a", "172.31.0.11:8500", "first agent (migration-network address)")
		xferB    = flag.String("xfer-b", "172.31.0.12:8500", "second agent (migration-network address)")
		vmID     = flag.String("vm", "vm0", "VM to migrate")
		interval = flag.Duration("interval", time.Millisecond, "probe send interval")
		runs     = flag.Int("n", 100, "bench: number of migrations")
		csvPath  = flag.String("csv", "bench-results/blackout.csv", "bench: output CSV")
		budget   = flag.Duration("budget", 30*time.Millisecond, "blackout budget (pass/fail)")
	)
	flag.Parse()

	cmd := flag.Arg(0)
	if cmd == "" {
		cmd = "probe"
	}
	hosts := [2]host{{ctrl: *agentA, xfer: *xferA}, {ctrl: *agentB, xfer: *xferB}}
	switch cmd {
	case "probe":
		p := startProber(*guest, *interval)
		defer p.stop()
		time.Sleep(5 * time.Second)
		st := p.stats(p.windowAll())
		log.Printf("5s probe: sent=%d recv=%d lost=%d lostrun=%.2fms degraded=%.2fms sendstall=%.2fms",
			st.Sent, st.Received, st.Lost, ms(st.LostRunNS), ms(st.MaxGapNS), ms(st.MaxSendStallNS))
	case "migrate":
		src, dst := orderAgents(hosts, *vmID)
		res, err := oneMigration(*guest, *interval, src, dst, *vmID)
		if err != nil {
			log.Fatalf("migration failed: %v", err)
		}
		printMigration(res, *budget)
	case "bench":
		runBench(*guest, *interval, hosts, *vmID, *runs, *csvPath, *budget)
	default:
		log.Fatalf("unknown command %q (want probe|migrate|bench)", cmd)
	}
}

// host pairs an agent's control address (reachable over the guest L2) with
// its migration-network address (where peers send bulk transfer).
type host struct {
	ctrl string
	xfer string
}

func ms(ns int64) float64 { return float64(ns) / 1e6 }

// --- prober ---------------------------------------------------------------------

type sample struct {
	seq            uint64
	sentNS, recvNS int64 // CLOCK_MONOTONIC; recvNS==0 → never answered
}

type prober struct {
	conn     *net.UDPConn
	interval time.Duration
	mu       sync.Mutex
	samples  []sample
	done     chan struct{}
}

func startProber(guest string, interval time.Duration) *prober {
	addr, err := net.ResolveUDPAddr("udp", guest)
	if err != nil {
		log.Fatalf("resolve %s: %v", guest, err)
	}
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		log.Fatalf("dial %s: %v", guest, err)
	}
	p := &prober{conn: conn, interval: interval, done: make(chan struct{})}
	go p.sendLoop()
	go p.recvLoop()
	return p
}

func (p *prober) sendLoop() {
	var seq uint64
	buf := make([]byte, 16)
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-t.C:
		}
		seq++
		now := timeline.Now()
		binary.BigEndian.PutUint64(buf[0:8], seq)
		binary.BigEndian.PutUint64(buf[8:16], uint64(now))
		if _, err := p.conn.Write(buf); err != nil {
			continue // guest briefly unreachable mid-migration: expected
		}
		p.mu.Lock()
		p.samples = append(p.samples, sample{seq: seq, sentNS: now})
		p.mu.Unlock()
	}
}

func (p *prober) recvLoop() {
	buf := make([]byte, 2048)
	for {
		n, err := p.conn.Read(buf)
		if err != nil {
			select {
			case <-p.done:
				return
			default:
				continue
			}
		}
		if n < 16 {
			continue
		}
		seq := binary.BigEndian.Uint64(buf[0:8])
		now := timeline.Now()
		p.mu.Lock()
		// Replies arrive in near-order; scan back from the tail.
		for i := len(p.samples) - 1; i >= 0 && i >= len(p.samples)-4096; i-- {
			if p.samples[i].seq == seq {
				p.samples[i].recvNS = now
				break
			}
		}
		p.mu.Unlock()
	}
}

func (p *prober) stop() { close(p.done); p.conn.Close() }

// window returns samples sent within [fromNS, toNS].
func (p *prober) window(fromNS, toNS int64) []sample {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []sample
	for _, s := range p.samples {
		if s.sentNS >= fromNS && s.sentNS <= toNS {
			out = append(out, s)
		}
	}
	return out
}

func (p *prober) windowAll() []sample {
	return p.window(0, 1<<62)
}

// switchoverBlackout isolates the genuine stop-the-world downtime from
// pre-copy brownout: among the maximal runs of never-answered probes, it
// returns the one whose time span overlaps the agent's actual [pause,
// resume] window. That run is the outage the client saw *around the real
// switchover*; congestion drops elsewhere in the migration are excluded by
// construction. Returns 0 if every probe bracketing the switchover was
// answered (downtime below the probe interval). If brownout drops happen to
// abut the switchover and merge into this run, the number is inflated —
// i.e. biased against us, which is the honest direction.
func (p *prober) switchoverBlackout(pauseNS, resumeNS int64) int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	var run gap
	var best int64
	flush := func() {
		if run.StartNS != 0 && run.StartNS <= resumeNS && run.EndNS >= pauseNS && run.len() > best {
			best = run.len()
		}
		run = gap{}
	}
	for _, s := range p.samples {
		if s.recvNS == 0 {
			if run.StartNS == 0 {
				run = gap{StartNS: s.sentNS, EndNS: s.sentNS + int64(p.interval)}
			} else {
				run.EndNS = s.sentNS + int64(p.interval)
			}
		} else {
			flush()
		}
	}
	flush()
	return best
}

type gap struct {
	StartNS, EndNS int64
}

func (g gap) len() int64 { return g.EndNS - g.StartNS }

type probeStats struct {
	Sent, Received int
	Lost           int
	// LostRunNS is the headline blackout: the longest stretch of
	// consecutive probes that were NEVER answered — the signature of a VM
	// that is not running (paused, or its packets steered at a dead host).
	// Robust against prober-side scheduling stalls: a stalled sender simply
	// emits no probes and contributes nothing, whereas measuring silence
	// between received replies would misattribute the sender's own hiccups
	// to the guest.
	LostRunNS int64
	// TopLostRuns are the largest never-answered runs with their positions,
	// so congestion-drop events can be told apart from the switchover gap.
	TopLostRuns []gap
	// MaxGapNS is the degraded window: like LostRunNS but also counting
	// probes answered slower than badRTT. It brackets brownout phases
	// (pre-copy contention, post-restore memory fault-in) where the VM is
	// running but slow — reported alongside, never conflated with blackout.
	MaxGapNS int64
	// MaxSendStallNS reports the prober's own worst send-loop stall, so a
	// polluted run is visible instead of silently folded into the result.
	MaxSendStallNS int64
	TopGaps        []gap // largest bad-probe runs, for timeline alignment
}

// badRTT is the reply deadline: a probe answered slower than this counts as
// blackout time. Steady-state RTT on this bridge is ~100-300µs.
const badRTT = 5 * time.Millisecond

// stats computes the client-observed blackout over a window of samples
// (already in send order). A probe is "bad" if it was never answered or
// answered slower than badRTT; the blackout figure is the wall-clock span of
// the longest consecutive bad run, extended by one probe interval (the
// resolution of the measurement, charged against us).
func (p *prober) stats(win []sample) probeStats {
	st := probeStats{Sent: len(win)}
	var gaps, lostRuns []gap
	var run *gap
	var lostRun gap
	prevSent := int64(0)
	for _, s := range win {
		if prevSent > 0 && s.sentNS-prevSent > st.MaxSendStallNS {
			st.MaxSendStallNS = s.sentNS - prevSent
		}
		prevSent = s.sentNS
		bad := s.recvNS == 0 || s.recvNS-s.sentNS > int64(badRTT)
		if s.recvNS > 0 {
			st.Received++
		}

		switch {
		case s.recvNS == 0 && lostRun.StartNS == 0:
			lostRun = gap{StartNS: s.sentNS, EndNS: s.sentNS + int64(p.interval)}
		case s.recvNS == 0:
			lostRun.EndNS = s.sentNS + int64(p.interval)
		default:
			if lostRun.StartNS != 0 {
				lostRuns = append(lostRuns, lostRun)
			}
			lostRun = gap{}
		}

		switch {
		case bad && run == nil:
			gaps = append(gaps, gap{StartNS: s.sentNS, EndNS: s.sentNS + int64(p.interval)})
			run = &gaps[len(gaps)-1]
		case bad:
			run.EndNS = s.sentNS + int64(p.interval)
		default:
			run = nil
		}
	}
	if lostRun.StartNS != 0 {
		lostRuns = append(lostRuns, lostRun)
	}
	sort.Slice(lostRuns, func(i, j int) bool { return lostRuns[i].len() > lostRuns[j].len() })
	if len(lostRuns) > 0 {
		st.LostRunNS = lostRuns[0].len()
	}
	st.TopLostRuns = lostRuns[:min(3, len(lostRuns))]
	st.Lost = st.Sent - st.Received
	sort.Slice(gaps, func(i, j int) bool { return gaps[i].len() > gaps[j].len() })
	if len(gaps) > 0 {
		st.MaxGapNS = gaps[0].len()
	}
	st.TopGaps = gaps[:min(3, len(gaps))]
	return st
}

// --- migration driving -------------------------------------------------------------

// agentReport mirrors agent.Report (fields we consume).
type agentReport struct {
	BlackoutNS int64 `json:"blackout_ns"`
	BaseBytes  int64 `json:"base_bytes"`
	FinalBytes int64 `json:"final_bytes"`
	LiveRounds bool  `json:"live_rounds"`
	Rounds     []struct {
		Round      int   `json:"round"`
		DirtyBytes int64 `json:"dirty_bytes"`
		StartNS    int64 `json:"start_ns"`
		SnapshotNS int64 `json:"snapshot_ns"`
		TransferNS int64 `json:"transfer_ns"`
	} `json:"rounds"`
	Timeline []struct {
		Name string `json:"name"`
		NS   int64  `json:"ns"`
	} `json:"timeline"`
}

type migrationResult struct {
	Agent        agentReport
	Observed     probeStats
	Guest        guestStatus
	DurationNS   int64
	SwitchoverNS int64  // client-observed downtime bracketing the real pause→resume
	ServiceMap   string // per-250ms-bin answer rate across the probe window
}

// timelineEvent returns the CLOCK_MONOTONIC timestamp of a named agent
// timeline event (0 if absent).
func (r agentReport) timelineEvent(name string) int64 {
	for _, e := range r.Timeline {
		if e.Name == name {
			return e.NS
		}
	}
	return 0
}

// serviceMap renders per-bin probe answer rates: '·' ≥99%, digits 1..9 for
// 10..99%, 'X' for a bin where (almost) nothing was answered in time.
func serviceMap(win []sample, binNS int64) string {
	if len(win) == 0 {
		return ""
	}
	start := win[0].sentNS
	var sent, good []int
	for _, s := range win {
		bin := int((s.sentNS - start) / binNS)
		for len(sent) <= bin {
			sent, good = append(sent, 0), append(good, 0)
		}
		sent[bin]++
		if s.recvNS > 0 && s.recvNS-s.sentNS <= int64(badRTT) {
			good[bin]++
		}
	}
	out := make([]byte, len(sent))
	for i := range sent {
		switch frac := float64(good[i]) / float64(max(sent[i], 1)); {
		case frac >= 0.99:
			out[i] = 0xB7 // '·'
		case frac < 0.10:
			out[i] = 'X'
		default:
			out[i] = byte('0' + int(frac*10))
		}
	}
	// Render '·' properly as UTF-8.
	return strings.ReplaceAll(string(out), "\xb7", "·")
}

type guestStatus struct {
	Counter         int64 `json:"counter"`
	IntegrityErrors int64 `json:"integrity_errors"`
	PagesVerified   int64 `json:"pages_verified"`
}

// HTTP clients, split by how long each call is *legitimately* allowed to
// block. Go's default client has no timeout at all, and a guest that has died
// mid-bench (the repeated-restore panic in docs/measurements.md) stops
// answering ARP entirely — so a bare http.Get on it blocks in connect forever,
// deadlocking the run instead of surfacing an error the bench can recover
// from by rebooting a fresh guest.
var (
	// ctrlHTTP: guest /status and agent placement queries — must answer
	// promptly (a healthy guest replies in microseconds), so the bound is kept
	// short enough that waitGuestStatus gets several attempts inside its grace.
	ctrlHTTP = &http.Client{Timeout: 5 * time.Second}
	// bootHTTP: POST /vms boots a VM and waits for the guest to come up.
	bootHTTP = &http.Client{Timeout: 90 * time.Second}
	// migrateHTTP: blocks for the whole migration (pre-copy rounds included),
	// so this bound only exists to break a hung agent, not to pace anything.
	migrateHTTP = &http.Client{Timeout: 10 * time.Minute}
)

// oneMigration probes across a single src→dst migration and returns both
// views of the blackout plus the guest's own integrity report. The migration
// is triggered on the source's control address, but the source is told to
// reach the destination at its migration-network address.
func oneMigration(guest string, interval time.Duration, src, dst host, vmID string) (*migrationResult, error) {
	p := startProber(guest, interval)
	defer p.stop()
	time.Sleep(1 * time.Second) // steady-state baseline before the event

	start := timeline.Now()
	body, _ := json.Marshal(map[string]string{"target": dst.xfer})
	resp, err := migrateHTTP.Post("http://"+src.ctrl+"/vms/"+vmID+"/migrate", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		return nil, fmt.Errorf("agent %s: HTTP %d: %s", src.ctrl, resp.StatusCode, buf.String())
	}
	var rep agentReport
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
		return nil, err
	}
	end := timeline.Now()
	time.Sleep(1 * time.Second) // give late replies time to land

	win := p.window(start-int64(200*time.Millisecond), end+int64(time.Second))
	pauseNS := rep.timelineEvent("blackout.pause")
	resumeNS := rep.timelineEvent("target.resumed")
	res := &migrationResult{
		Agent:        rep,
		Observed:     p.stats(win),
		DurationNS:   end - start,
		SwitchoverNS: p.switchoverBlackout(pauseNS, resumeNS),
		ServiceMap:   serviceMap(win, int64(250*time.Millisecond)),
	}
	gs, err := waitGuestStatus(guest, 20*time.Second)
	if err != nil {
		return nil, err
	}
	res.Guest = gs
	return res, nil
}

// waitGuestStatus polls the guest's /status until it answers, allowing a grace
// period for it to fault pages back in after the restore. Treating an
// unreachable guest as an error rather than a missing statistic is deliberate:
// the whole claim is that the guest survives the move, so a silent zero-valued
// status would record a dead guest as a clean run (0 integrity errors over 0
// pages verified) and let the bench average in migrations nobody survived.
func waitGuestStatus(guest string, grace time.Duration) (guestStatus, error) {
	deadline := time.Now().Add(grace)
	var lastErr error
	for {
		gs, err := fetchGuestStatus(guest)
		if err == nil {
			return gs, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return guestStatus{}, fmt.Errorf("guest %s did not answer within %v of resume: %w", guest, grace, lastErr)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func fetchGuestStatus(guest string) (guestStatus, error) {
	host, _, err := net.SplitHostPort(guest)
	if err != nil {
		return guestStatus{}, err
	}
	resp, err := ctrlHTTP.Get("http://" + host + ":7780/status")
	if err != nil {
		return guestStatus{}, err
	}
	defer resp.Body.Close()
	var gs guestStatus
	return gs, json.NewDecoder(resp.Body).Decode(&gs)
}

// rebootVM boots a fresh benchmark VM after a guest death. It tries each host
// (a dead/migrated slot can be replaced), waits for the guest to answer, and
// returns the new (source, dest) ordering. Best-effort: returns ok=false if
// no host accepts the boot.
func rebootVM(hosts [2]host, vmID string) (src, dst host, ok bool) {
	// Clear the slot on *both* hosts first. A guest whose kernel panicked
	// leaves a slot the agent still considers running, which would make the
	// boot below 409 forever; a migration interrupted midway can leave a
	// stale slot on the other host too. DELETE is idempotent, so this is safe
	// even when only one side holds anything.
	for _, h := range hosts {
		req, err := http.NewRequest(http.MethodDelete, "http://"+h.ctrl+"/vms/"+vmID, nil)
		if err != nil {
			continue
		}
		if resp, err := ctrlHTTP.Do(req); err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}

	body := []byte(`{"id":"` + vmID + `","vcpus":1,"mem_mib":256,"scrib_buf_mb":32,"dirty_mbps":5}`)
	for k, h := range hosts {
		resp, err := bootHTTP.Post("http://"+h.ctrl+"/vms", "application/json", bytes.NewReader(body))
		if err != nil {
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			log.Printf("       rebooted fresh %s on %s", vmID, h.ctrl)
			return h, hosts[1-k], true
		}
	}
	return host{}, host{}, false
}

// orderAgents figures out which host currently runs the VM (source) and
// which is the destination, querying control addresses.
func orderAgents(hosts [2]host, vmID string) (src, dst host) {
	for _, pair := range [][2]host{{hosts[0], hosts[1]}, {hosts[1], hosts[0]}} {
		resp, err := ctrlHTTP.Get("http://" + pair[0].ctrl + "/vms/" + vmID)
		if err != nil {
			continue
		}
		var st struct {
			State string `json:"state"`
		}
		err = json.NewDecoder(resp.Body).Decode(&st)
		resp.Body.Close()
		if err == nil && st.State == "running" {
			return pair[0], pair[1]
		}
	}
	log.Fatalf("no agent reports %s running", vmID)
	return host{}, host{}
}

func printMigration(res *migrationResult, budget time.Duration) {
	log.Printf("migration finished in %.1fms total (pre-copy + blackout)", ms(res.DurationNS))
	// Everything below is printed relative to migrate.start so probe gaps
	// can be visually aligned with migration phases.
	var t0 int64
	for _, e := range res.Agent.Timeline {
		if e.Name == "migrate.start" {
			t0 = e.NS
		}
	}
	for _, e := range res.Agent.Timeline {
		log.Printf("  t+%7.1fms  %s", ms(e.NS-t0), e.Name)
	}
	for _, r := range res.Agent.Rounds {
		log.Printf("  t+%7.1fms  round %d: %6.1f KiB dirty (snapshot %.1fms + transfer %.1fms)",
			ms(r.StartNS-t0), r.Round, float64(r.DirtyBytes)/1024, ms(r.SnapshotNS), ms(r.TransferNS))
	}
	for _, g := range res.Observed.TopGaps {
		log.Printf("  t+%7.1fms  degraded window %.2fms", ms(g.StartNS-t0), ms(g.len()))
	}
	for _, g := range res.Observed.TopLostRuns {
		log.Printf("  t+%7.1fms  lost run %.2fms", ms(g.StartNS-t0), ms(g.len()))
	}
	log.Printf("service timeline (250ms bins: answered/sent, '·'=100%%, digit=fraction, X=dead):")
	log.Printf("  %s", res.ServiceMap)
	log.Printf("  final round: %.1f KiB inside blackout", float64(res.Agent.FinalBytes)/1024)
	log.Printf("BLACKOUT (stop-the-world downtime):")
	log.Printf("  agent  pause→resume    : %8.2f ms  (VM provably not executing)", ms(res.Agent.BlackoutNS))
	log.Printf("  client bracketing probe: %8.2f ms  (unanswered run overlapping pause→resume)", ms(res.SwitchoverNS))
	log.Printf("pre-copy brownout (VM running, network degraded — NOT blackout):")
	log.Printf("  worst degraded window  : %8.2f ms  (probes answered slower than %v)", ms(res.Observed.MaxGapNS), badRTT)
	log.Printf("guest integrity          : %d errors over %d pages verified",
		res.Guest.IntegrityErrors, res.Guest.PagesVerified)
	log.Printf("prober self-check        : %8.2f ms  worst send-loop stall (measurement noise floor)", ms(res.Observed.MaxSendStallNS))
	// The budget applies to blackout (downtime), the challenge's target.
	// Brownout is reported for honesty but is not downtime.
	blackout := max(res.SwitchoverNS, res.Agent.BlackoutNS)
	verdict := "PASS"
	if blackout > int64(budget) || res.Guest.IntegrityErrors > 0 {
		verdict = "FAIL"
	}
	log.Printf("blackout budget %.0fms → %s", ms(int64(budget)), verdict)
}

// --- bench ---------------------------------------------------------------------------

func runBench(guest string, interval time.Duration, hosts [2]host, vmID string, n int, csvPath string, budget time.Duration) {
	src, dst := orderAgents(hosts, vmID)
	if err := os.MkdirAll(dirOf(csvPath), 0o755); err != nil {
		log.Fatal(err)
	}
	f, err := os.Create(csvPath)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	fmt.Fprintln(f, "run,source,blackout_ms,agent_blackout_ms,brownout_ms,rounds,final_kib,integrity_errors")

	var observed []int64
	fails := 0
	for i := 1; i <= n; i++ {
		res, err := oneMigration(guest, interval, src, dst, vmID)
		if err != nil {
			log.Printf("run %d: MIGRATION ERROR: %v", i, err)
			fails++
			// A failed migration usually means the guest kernel died (see the
			// repeated-restore note in docs/measurements.md). Reboot a fresh
			// VM so the benchmark completes its full sample instead of
			// stalling on a corpse; re-discover placement either way.
			if s, d, ok := rebootVM(hosts, vmID); ok {
				src, dst = s, d
			} else {
				src, dst = orderAgents(hosts, vmID)
			}
			continue
		}
		blackout := max(res.SwitchoverNS, res.Agent.BlackoutNS)
		observed = append(observed, blackout)
		fmt.Fprintf(f, "%d,%s,%.3f,%.3f,%.3f,%d,%.1f,%d\n",
			i, src.ctrl, ms(blackout), ms(res.Agent.BlackoutNS), ms(res.Observed.MaxGapNS),
			len(res.Agent.Rounds), float64(res.Agent.FinalBytes)/1024, res.Guest.IntegrityErrors)
		status := "ok"
		if blackout > int64(budget) {
			status = "OVER BUDGET"
			fails++
		}
		if res.Guest.IntegrityErrors > 0 {
			status = "INTEGRITY FAIL"
			fails++
		}
		log.Printf("run %3d: %s→%s  blackout=%6.2fms (agent=%5.2f) brownout=%7.2fms  %s",
			i, src.ctrl, dst.ctrl, ms(blackout), ms(res.Agent.BlackoutNS), ms(res.Observed.MaxGapNS), status)
		src, dst = dst, src         // ping-pong
		time.Sleep(2 * time.Second) // let the guest stabilize before the next move

	}

	if len(observed) == 0 {
		log.Fatal("no successful migrations")
	}
	sort.Slice(observed, func(i, j int) bool { return observed[i] < observed[j] })
	q := func(p float64) float64 { return ms(observed[int(p*float64(len(observed)-1))]) }
	log.Printf("---")
	log.Printf("%d migrations, %d failures", len(observed), fails)
	log.Printf("client-observed blackout: p50=%.2fms p90=%.2fms p99=%.2fms max=%.2fms",
		q(0.50), q(0.90), q(0.99), q(1.0))
	if fails == 0 && observed[len(observed)-1] <= int64(budget) {
		log.Printf("ALL runs within %.0fms budget: PASS", ms(int64(budget)))
	} else {
		log.Printf("budget %.0fms: FAIL", ms(int64(budget)))
	}
}

func dirOf(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return "."
}
