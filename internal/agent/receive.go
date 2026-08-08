//go:build linux

package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/lgoyal6/fc-live-migration/internal/sparse"
	"github.com/lgoyal6/fc-live-migration/internal/timeline"
)

// Target-side handlers. The source agent drives these in order:
//
//	prepare  → spawn an idle Firecracker process, create the memory file
//	mem      → splat base + pre-copy rounds into the memory file (repeated)
//	vmstate  → store the final device/vCPU state file
//	finalize → snapshot-load + resume + gratuitous ARP (the blackout tail)
//	abort    → tear everything down; source VM keeps running
//
// Everything expensive happens before finalize, while the guest is still
// running on the source.

func (a *Agent) handlePrepare(w http.ResponseWriter, r *http.Request) {
	var cfg VMConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		http.Error(w, "bad VMConfig: "+err.Error(), http.StatusBadRequest)
		return
	}
	if cfg.ID != r.PathValue("id") {
		http.Error(w, "config/path ID mismatch", http.StatusBadRequest)
		return
	}

	vm := &VM{Cfg: cfg, dir: a.vmDir(cfg.ID), state: StateReceiving}
	if err := vm.spawnFirecracker(r.Context(), a.cfg.FirecrackerBin, a.throttle); err != nil {
		http.Error(w, "spawn firecracker: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// The incoming memory image accumulates base + rounds in place.
	f, err := os.OpenFile(vm.recvMemPath(), os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		vm.kill()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f.Close()

	if old := a.putVM(vm); old != nil {
		old.kill() // a re-prepare replaces a stale receiving slot
	}
	a.events.publish("receive.prepared", cfg.ID)
	w.WriteHeader(http.StatusOK)
}

func (a *Agent) handleMem(w http.ResponseWriter, r *http.Request) {
	vm := a.getVM(r.PathValue("id"))
	if vm == nil || vm.State() != StateReceiving {
		http.Error(w, "no VM in receiving state", http.StatusConflict)
		return
	}
	f, err := os.OpenFile(vm.recvMemPath(), os.O_RDWR, 0o644)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()

	n, err := sparse.Receive(bufio.NewReaderSize(r.Body, 256<<10), f)
	if err != nil {
		http.Error(w, "receive stream: "+err.Error(), http.StatusBadRequest)
		return
	}
	a.events.publish("receive.mem",
		fmt.Sprintf("%s round=%s bytes=%d", vm.Cfg.ID, r.URL.Query().Get("round"), n))
	writeJSON(w, map[string]int64{"bytes": n})
}

func (a *Agent) handleVMState(w http.ResponseWriter, r *http.Request) {
	vm := a.getVM(r.PathValue("id"))
	if vm == nil || vm.State() != StateReceiving {
		http.Error(w, "no VM in receiving state", http.StatusConflict)
		return
	}
	f, err := os.Create(vm.finalStatePath())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	if _, err := io.Copy(f, r.Body); err != nil {
		http.Error(w, "store vmstate: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleFinalize is the target half of the blackout window: load the
// snapshot (memory file is hot in the page cache — we just wrote it),
// resume, and announce the guest's new location to the L2.
func (a *Agent) handleFinalize(w http.ResponseWriter, r *http.Request) {
	vm := a.getVM(r.PathValue("id"))
	if vm == nil || vm.State() != StateReceiving {
		http.Error(w, "no VM in receiving state", http.StatusConflict)
		return
	}

	rec := &timeline.Recorder{}
	rec.Mark("target.load_start")
	if err := vm.api.LoadSnapshot(vm.finalStatePath(), vm.recvMemPath(), true, true); err != nil {
		http.Error(w, "snapshot load: "+err.Error(), http.StatusInternalServerError)
		return
	}
	resumedNS := rec.Mark("target.resumed")

	// Gratuitous ARP from the guest's MAC so the upstream bridge re-learns
	// which port the guest lives behind. Sent on the enslaved uplink (not
	// br0) to avoid polluting our own bridge's FDB with a local entry.
	guestIP, _, _ := splitCIDR(vm.Cfg.GuestCIDR)
	if err := sendGARP(a.cfg.GARPIface, vm.Cfg.GuestMAC, guestIP); err != nil {
		// The guest's own egress traffic will re-teach the bridge shortly;
		// GARP just makes the flip deterministic. Log, don't fail.
		a.events.publish("garp.error", err.Error())
	}
	rec.Mark("target.garp_sent")

	// The received image is this VM's full memory state as of the final
	// pause: it is the base for the *next* migration (ping-pong).
	vm.baseMem = vm.recvMemPath()
	vm.setState(StateRunning)
	a.events.publish("receive.resumed", vm.Cfg.ID)

	writeJSON(w, finalizeResponse{ResumedNS: resumedNS, Timeline: rec.Events()})
}

func (a *Agent) handleAbort(w http.ResponseWriter, r *http.Request) {
	vm := a.getVM(r.PathValue("id"))
	if vm == nil {
		w.WriteHeader(http.StatusOK) // nothing to do
		return
	}
	if vm.State() != StateReceiving {
		http.Error(w, "refusing to abort a VM not in receiving state", http.StatusConflict)
		return
	}
	vm.kill()
	a.deleteVM(vm.Cfg.ID)
	// Keep the VMM's log out of the teardown — if the abort was caused by
	// the firecracker process dying, that log is the only evidence.
	preserved := filepath.Join(a.cfg.DataDir, fmt.Sprintf("%s-fc.log.aborted", vm.Cfg.ID))
	_ = os.Rename(vm.logPath(), preserved)
	_ = os.RemoveAll(vm.dir)
	a.events.publish("receive.aborted", fmt.Sprintf("%s (vmm log: %s)", vm.Cfg.ID, preserved))
	w.WriteHeader(http.StatusOK)
}
