//go:build linux

// Package agent implements migrated, the live-migration control plane that
// runs on every host. It owns local Firecracker processes and exposes:
//
//	POST   /vms              — boot a microVM on this host
//	GET    /vms/{id}         — inspect a VM slot
//	DELETE /vms/{id}         — kill a VM and free its slot
//	POST   /vms/{id}/migrate — live-migrate a VM to another host's agent
//	GET  /events             — SSE stream of lifecycle/migration events
//	POST /internal/vms/{id}/(prepare|mem|vmstate|finalize|abort)
//	                         — target-side migration protocol (agent→agent)
package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"

	"github.com/lgoyal6/fc-live-migration/internal/timeline"
)

// Config is the agent's static configuration (flags in cmd/migrated).
type Config struct {
	Name           string // host name used in logs/events, e.g. "host-a"
	Listen         string // HTTP listen address
	DataDir        string // tmpfs runtime dir for sockets + snapshot files
	FirecrackerBin string
	Kernel         string // default guest kernel (shared storage)
	Rootfs         string // default guest rootfs (shared storage)
	GARPIface      string // uplink to announce migrated MACs on
	LiveRounds     bool   // use the DiffLive patch for pause-free pre-copy
}

type Agent struct {
	cfg        Config
	mu         sync.Mutex
	vms        map[string]*VM
	throttle   *Throttler
	events     *eventHub
	peerClient *http.Client
}

func New(cfg Config) *Agent {
	return &Agent{
		cfg:        cfg,
		vms:        map[string]*VM{},
		throttle:   NewThrottler(),
		events:     newEventHub(cfg.Name),
		peerClient: peerHTTPClient(),
	}
}

// ListenAndServe blocks serving the agent API.
func (a *Agent) ListenAndServe() error {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /vms", a.handleCreateVM)
	mux.HandleFunc("GET /vms/{id}", a.handleGetVM)
	mux.HandleFunc("DELETE /vms/{id}", a.handleDeleteVM)
	mux.HandleFunc("POST /vms/{id}/migrate", a.handleMigrate)
	mux.HandleFunc("GET /events", a.events.serveSSE)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /internal/vms/{id}/prepare", a.handlePrepare)
	mux.HandleFunc("POST /internal/vms/{id}/mem", a.handleMem)
	mux.HandleFunc("POST /internal/vms/{id}/vmstate", a.handleVMState)
	mux.HandleFunc("POST /internal/vms/{id}/finalize", a.handleFinalize)
	mux.HandleFunc("POST /internal/vms/{id}/abort", a.handleAbort)

	fmt.Printf("migrated %s listening on %s (live_rounds=%v)\n",
		a.cfg.Name, a.cfg.Listen, a.cfg.LiveRounds)
	return http.ListenAndServe(a.cfg.Listen, mux)
}

// --- VM registry ------------------------------------------------------------

func (a *Agent) vmDir(id string) string { return filepath.Join(a.cfg.DataDir, id) }

func (a *Agent) getVM(id string) *VM {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.vms[id]
}

// putVM installs a VM slot, returning any previous occupant.
func (a *Agent) putVM(vm *VM) *VM {
	a.mu.Lock()
	defer a.mu.Unlock()
	old := a.vms[vm.Cfg.ID]
	a.vms[vm.Cfg.ID] = vm
	return old
}

func (a *Agent) deleteVM(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.vms, id)
}

// --- Public API handlers ------------------------------------------------------

// CreateVMRequest lets callers override defaults; only ID is required.
type CreateVMRequest struct {
	ID         string `json:"id"`
	Vcpus      int    `json:"vcpus"`
	MemMiB     int    `json:"mem_mib"`
	GuestMAC   string `json:"guest_mac"`
	GuestCIDR  string `json:"guest_cidr"`
	GuestGW    string `json:"guest_gw"`
	DirtyMBps  int    `json:"dirty_mbps"`
	ScribBufMB int    `json:"scrib_buf_mb"`
}

func (a *Agent) handleCreateVM(w http.ResponseWriter, r *http.Request) {
	var req CreateVMRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.ID == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}
	cfg := VMConfig{
		ID:    req.ID,
		Vcpus: orDefault(req.Vcpus, 2),
		// 2 vCPUs so the guest's service goroutines aren't hostage to the
		// post-restore fault storm hitting whichever vCPU runs the scribbler.
		MemMiB:     orDefault(req.MemMiB, 256),
		Kernel:     a.cfg.Kernel,
		Rootfs:     a.cfg.Rootfs,
		TapDev:     "tap0",
		GuestMAC:   orDefaultStr(req.GuestMAC, "06:00:ac:1e:00:32"),
		GuestCIDR:  orDefaultStr(req.GuestCIDR, "172.30.0.50/24"),
		GuestGW:    orDefaultStr(req.GuestGW, "172.30.0.1"),
		DirtyMBps:  req.DirtyMBps,
		ScribBufMB: orDefault(req.ScribBufMB, 64),
	}

	vm := &VM{Cfg: cfg, dir: a.vmDir(cfg.ID)}
	if old := a.putVM(vm); old != nil && old.State() != StateMigrated && old.State() != StateDead {
		a.deleteVM(cfg.ID)
		http.Error(w, "vm already exists: "+cfg.ID, http.StatusConflict)
		return
	}
	if err := vm.boot(r.Context(), a.cfg.FirecrackerBin, a.throttle); err != nil {
		vm.kill()
		vm.setState(StateDead)
		http.Error(w, "boot: "+err.Error(), http.StatusInternalServerError)
		return
	}
	a.events.publish("vm.running", cfg.ID)
	a.writeVMStatus(w, vm)
}

func (a *Agent) handleGetVM(w http.ResponseWriter, r *http.Request) {
	vm := a.getVM(r.PathValue("id"))
	if vm == nil {
		http.Error(w, "no such vm", http.StatusNotFound)
		return
	}
	a.writeVMStatus(w, vm)
}

// handleDeleteVM tears a VM down and frees its slot. This exists for the case
// the VMM cannot detect on its own: the *guest kernel* dies (the repeated-
// restore panic in docs/measurements.md) while Firecracker itself stays
// healthy and the slot stays StateRunning. Nothing can then reclaim the ID —
// POST /vms would 409 forever — so the bench harness deletes the corpse before
// booting a fresh guest. Idempotent: deleting an unknown ID succeeds.
func (a *Agent) handleDeleteVM(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if vm := a.getVM(id); vm != nil {
		vm.kill()
		vm.setState(StateDead)
		a.events.publish("vm.deleted", id)
	}
	a.deleteVM(id)
	w.WriteHeader(http.StatusNoContent)
}

// MigrateRequest names the destination agent ("host:port").
type MigrateRequest struct {
	Target string `json:"target"`
}

func (a *Agent) handleMigrate(w http.ResponseWriter, r *http.Request) {
	vm := a.getVM(r.PathValue("id"))
	if vm == nil {
		http.Error(w, "no such vm", http.StatusNotFound)
		return
	}
	var req MigrateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Target == "" {
		http.Error(w, "target is required", http.StatusBadRequest)
		return
	}
	// Migration outlives the HTTP request's default context only as long as
	// the client waits; an abandoned request aborts cleanly via migrate()'s
	// rollback path.
	rep, err := a.migrate(r.Context(), vm, req.Target)
	if err != nil {
		http.Error(w, "migrate: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, rep)
}

func (a *Agent) writeVMStatus(w http.ResponseWriter, vm *VM) {
	writeJSON(w, map[string]any{
		"id":    vm.Cfg.ID,
		"state": vm.State(),
		"host":  a.cfg.Name,
		"cfg":   vm.Cfg,
	})
}

// --- SSE event hub ------------------------------------------------------------

type eventHub struct {
	host string
	mu   sync.Mutex
	subs map[chan []byte]struct{}
}

func newEventHub(host string) *eventHub {
	return &eventHub{host: host, subs: map[chan []byte]struct{}{}}
}

// publish fans an event out to SSE subscribers (and the agent log). Slow
// subscribers lose events rather than block a migration.
func (h *eventHub) publish(kind, msg string) {
	e := map[string]any{"ns": timeline.Now(), "host": h.host, "kind": kind, "msg": msg}
	b, _ := json.Marshal(e)
	fmt.Printf("event %s\n", b)
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- b:
		default:
		}
	}
}

func (h *eventHub) serveSSE(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch := make(chan []byte, 256)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fl.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case b := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		}
	}
}

// --- small helpers -------------------------------------------------------------

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func orDefault(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func orDefaultStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
