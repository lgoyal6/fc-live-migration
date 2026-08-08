// guestd is the workload that runs inside the microVM (PID 1 execs it).
// It exists to make migration failures observable from the outside:
//
//   - UDP echo (:7777)     — the prober measures blackout as the longest
//     silence in this reply stream.
//   - TCP counter (:7788)  — a long-lived connection streaming a counter
//     that must neither reset nor disconnect across a migration.
//   - HTTP status (:7780)  — readiness + memory-integrity report.
//   - Scribbler            — dirties memory at a configurable rate and
//     self-verifies every page (index/iteration/checksum layout), so a torn
//     or lost page from a live pre-copy round shows up as a nonzero
//     integrity_errors after restore. Also the adversarial workload for the
//     auto-converge demo (guestd.dirty_mbps on the kernel command line, or
//     POST /dirty?mbps=N at runtime).
package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const pageSize = 4096

var (
	bootTime  = time.Now()
	counter   atomic.Int64 // TCP counter stream position; must survive migration
	dirtyMBps atomic.Int64
	scribbler *Scribbler
)

func main() {
	buf, rate := parseCmdline()
	dirtyMBps.Store(int64(rate))
	scribbler = NewScribbler(buf << 20)
	go scribbler.Run()
	go runUDPEcho(":7777")
	go runCounter(":7788")

	log.Printf("guestd up: scribbler=%dMiB dirty=%dMB/s", buf, rate)
	runStatus(":7780") // blocks; guestd exiting panics the guest by design
}

// parseCmdline reads guestd.buf_mb / guestd.dirty_mbps from /proc/cmdline.
func parseCmdline() (bufMB, mbps int) {
	bufMB = 64
	raw, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return
	}
	for _, tok := range strings.Fields(string(raw)) {
		if v, ok := strings.CutPrefix(tok, "guestd.buf_mb="); ok {
			bufMB, _ = strconv.Atoi(v)
		}
		if v, ok := strings.CutPrefix(tok, "guestd.dirty_mbps="); ok {
			mbps, _ = strconv.Atoi(v)
		}
	}
	return
}

// --- UDP echo -----------------------------------------------------------------

func runUDPEcho(addr string) {
	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		log.Fatalf("udp echo: %v", err)
	}
	buf := make([]byte, 2048)
	for {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			continue
		}
		_, _ = conn.WriteTo(buf[:n], from)
	}
}

// --- TCP counter stream ---------------------------------------------------------

func runCounter(addr string) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("counter: %v", err)
	}
	go func() {
		t := time.NewTicker(10 * time.Millisecond)
		for range t.C {
			counter.Add(1)
		}
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			continue
		}
		go func(c net.Conn) {
			defer c.Close()
			t := time.NewTicker(10 * time.Millisecond)
			defer t.Stop()
			for range t.C {
				if _, err := fmt.Fprintf(c, "count=%d uptime_ms=%d\n",
					counter.Load(), time.Since(bootTime).Milliseconds()); err != nil {
					return
				}
			}
		}(c)
	}
}

// --- status API ------------------------------------------------------------------

func runStatus(addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"uptime_ms":        time.Since(bootTime).Milliseconds(),
			"counter":          counter.Load(),
			"dirty_mbps":       dirtyMBps.Load(),
			"iterations":       scribbler.Iterations.Load(),
			"pages_verified":   scribbler.PagesVerified.Load(),
			"integrity_errors": scribbler.IntegrityErrors.Load(),
		})
	})
	mux.HandleFunc("POST /dirty", func(w http.ResponseWriter, r *http.Request) {
		mbps, err := strconv.Atoi(r.URL.Query().Get("mbps"))
		if err != nil || mbps < 0 {
			http.Error(w, "mbps must be a non-negative integer", http.StatusBadRequest)
			return
		}
		dirtyMBps.Store(int64(mbps))
		w.WriteHeader(http.StatusOK)
	})
	log.Fatal(http.ListenAndServe(addr, mux))
}

// --- scribbler --------------------------------------------------------------------

// Scribbler owns a buffer of self-describing pages. A single goroutine
// alternates writing and verifying (no intra-guest races), so any integrity
// error it ever reports was introduced by the migration pipeline.
type Scribbler struct {
	buf             []byte
	Iterations      atomic.Int64
	PagesVerified   atomic.Int64
	IntegrityErrors atomic.Int64
}

func NewScribbler(size int) *Scribbler {
	s := &Scribbler{buf: make([]byte, size)}
	for p := 0; p < len(s.buf)/pageSize; p++ {
		s.writePage(p, 0)
	}
	return s
}

func (s *Scribbler) Run() {
	rng := rand.New(rand.NewSource(1)) // determinism aids debugging
	pages := len(s.buf) / pageSize
	var iter int64
	for {
		iter++
		s.Iterations.Store(iter)

		// Dirty pages at the requested rate, in 1ms slices.
		perMS := int(dirtyMBps.Load()) * (1 << 20) / 1000 / pageSize
		if perMS == 0 {
			perMS = 1 // idle guests still tick, so diffs are never empty
		}
		start := time.Now()
		for i := 0; i < perMS; i++ {
			s.writePage(rng.Intn(pages), iter)
		}
		// Verify a sample of pages every iteration.
		for i := 0; i < 32; i++ {
			s.verifyPage(rng.Intn(pages))
		}
		if d := time.Millisecond - time.Since(start); d > 0 {
			time.Sleep(d)
		}
	}
}

// writePage fills page p with a layout fully determined by (p, iter):
// [ pageIdx u64 | iter u64 | keystream... ]. verifyPage can therefore
// recompute the expected bytes from the header alone; a page that is part
// old iteration, part new (a tear) can never verify.
//
// Both paths are allocation-free on purpose: this loop runs tens of
// thousands of times per second on the guest's single vCPU, and a heap
// allocation per page would drive the Go GC hard enough to stall the echo
// goroutine for tens of milliseconds — indistinguishable, from the prober's
// perspective, from migration blackout.
func (s *Scribbler) writePage(p int, iter int64) {
	pg := s.buf[p*pageSize : (p+1)*pageSize]
	binary.LittleEndian.PutUint64(pg[0:8], uint64(p))
	binary.LittleEndian.PutUint64(pg[8:16], uint64(iter))
	ks := keystream{state: uint64(p)<<32 ^ uint64(iter)}
	for off := 16; off < pageSize; off += 8 {
		binary.LittleEndian.PutUint64(pg[off:off+8], ks.next())
	}
}

func (s *Scribbler) verifyPage(p int) {
	pg := s.buf[p*pageSize : (p+1)*pageSize]
	idx := binary.LittleEndian.Uint64(pg[0:8])
	iter := binary.LittleEndian.Uint64(pg[8:16])
	ok := idx == uint64(p)
	ks := keystream{state: idx<<32 ^ iter}
	for off := 16; ok && off < pageSize; off += 8 {
		ok = binary.LittleEndian.Uint64(pg[off:off+8]) == ks.next()
	}
	s.PagesVerified.Add(1)
	if !ok {
		s.IntegrityErrors.Add(1)
		log.Printf("INTEGRITY ERROR page=%d header=(%d,%d)", p, idx, iter)
	}
}

// keystream is a splitmix64 sequence: deterministic, cheap, and seedable
// from a page's header alone.
type keystream struct{ state uint64 }

func (k *keystream) next() uint64 {
	k.state += 0x9e3779b97f4a7c15
	z := k.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}
