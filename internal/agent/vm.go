//go:build linux

package agent

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/lgoyal6/fc-live-migration/internal/fcapi"
)

// VMConfig is everything needed to boot (or receive) a microVM. It travels
// to the target agent during migration, so it must stay JSON-serializable.
type VMConfig struct {
	ID     string `json:"id"`
	Vcpus  int    `json:"vcpus"`
	MemMiB int    `json:"mem_mib"`

	// Kernel and Rootfs live on shared storage (the vmstore volume), the
	// standard shared-disk assumption of live migration: both hosts see the
	// same paths, so no disk data moves in the migration hot path.
	Kernel string `json:"kernel"`
	Rootfs string `json:"rootfs"`

	// TapDev must exist on both hosts (the containers create tap0 at start).
	// GuestMAC/GuestCIDR are baked into the snapshot, so the guest keeps its
	// L2/L3 identity across hosts — that is what lets TCP sessions survive.
	TapDev    string `json:"tap_dev"`
	GuestMAC  string `json:"guest_mac"`
	GuestCIDR string `json:"guest_cidr"` // e.g. 172.30.0.50/24
	GuestGW   string `json:"guest_gw"`

	// DirtyMBps throttles the guest's built-in memory scribbler (0 = idle
	// pattern). Passed on the kernel command line; used to demo convergence
	// under hostile dirty rates.
	DirtyMBps int `json:"dirty_mbps"`
	// ScribBufMB is the scribbler's working-set size.
	ScribBufMB int `json:"scrib_buf_mb"`
}

func (c *VMConfig) bootArgs() string {
	return fmt.Sprintf(
		"console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw "+
			"guestd.ip=%s guestd.gw=%s guestd.dirty_mbps=%d guestd.buf_mb=%d",
		c.GuestCIDR, c.GuestGW, c.DirtyMBps, c.ScribBufMB)
}

// VMState is the lifecycle of a VM slot on one host.
type VMState string

const (
	StateBooting   VMState = "booting"
	StateRunning   VMState = "running"
	StateMigrating VMState = "migrating"
	StateReceiving VMState = "receiving"
	StateMigrated  VMState = "migrated" // moved away; process is gone
	StateDead      VMState = "dead"
)

// VM is one microVM slot managed by this agent.
type VM struct {
	mu  sync.Mutex
	Cfg VMConfig

	state VMState
	dir   string // runtime dir on tmpfs: API socket, logs, snapshot files
	proc  *os.Process
	api   *fcapi.Client

	// baseMem is a full memory image the target already has (or will receive
	// first): the provision-time full snapshot for a booted VM, or the
	// restore image for a VM that arrived by migration. Pre-copy rounds are
	// diffs on top of it.
	baseMem string

	// needsResync is set when an aborted migration consumed dirty-page state
	// that was never applied anywhere; the next migration must re-establish
	// a fresh full base before doing rounds.
	needsResync bool
}

func (v *VM) State() VMState {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.state
}

func (v *VM) setState(s VMState) {
	v.mu.Lock()
	v.state = s
	v.mu.Unlock()
}

// paths inside the VM runtime dir.
func (v *VM) sockPath() string      { return filepath.Join(v.dir, "fc.sock") }
func (v *VM) logPath() string       { return filepath.Join(v.dir, "fc.log") }
func (v *VM) baseMemPath() string   { return filepath.Join(v.dir, "base.mem") }
func (v *VM) baseStatePath() string { return filepath.Join(v.dir, "base.vmstate") }
func (v *VM) roundMemPath(r int) string {
	return filepath.Join(v.dir, fmt.Sprintf("round-%d.mem", r))
}
func (v *VM) finalMemPath() string   { return filepath.Join(v.dir, "final.mem") }
func (v *VM) finalStatePath() string { return filepath.Join(v.dir, "final.vmstate") }
func (v *VM) recvMemPath() string    { return filepath.Join(v.dir, "recv.mem") }

// spawnFirecracker starts the VMM process with its API socket in v.dir and
// waits until the API answers. The process starts with no machine configured;
// the caller either boots it or restores a snapshot into it.
func (v *VM) spawnFirecracker(ctx context.Context, fcBin string, throttle *Throttler) error {
	if err := os.MkdirAll(v.dir, 0o755); err != nil {
		return err
	}
	_ = os.Remove(v.sockPath())

	logFile, err := os.Create(v.logPath())
	if err != nil {
		return err
	}
	defer logFile.Close()

	cmd := exec.Command(fcBin, "--api-sock", v.sockPath(), "--id", v.Cfg.ID)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start firecracker: %w", err)
	}
	v.proc = cmd.Process
	// Reap the child when it exits so we never accumulate zombies.
	go func() { _, _ = cmd.Process.Wait() }()

	if throttle != nil {
		if err := throttle.Register(v.Cfg.ID, cmd.Process.Pid); err != nil {
			// Throttling is an optimization (auto-converge); a VM that cannot
			// be throttled is still fully migratable.
			fmt.Fprintf(os.Stderr, "warn: throttle register for vm %s: %v\n", v.Cfg.ID, err)
		}
	}

	v.api = fcapi.New(v.sockPath())
	readyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return v.api.WaitReady(readyCtx)
}

// boot configures and starts a fresh guest, waits for the workload inside to
// answer, then captures the provision-time full snapshot that pre-copy
// rounds will diff against. The snapshot pause happens before the VM is
// reported ready, i.e. before it can be serving anyone.
func (v *VM) boot(ctx context.Context, fcBin string, throttle *Throttler) error {
	v.setState(StateBooting)
	if err := v.spawnFirecracker(ctx, fcBin, throttle); err != nil {
		return err
	}
	if err := v.api.PutMachineConfig(fcapi.MachineConfig{
		VcpuCount:       v.Cfg.Vcpus,
		MemSizeMiB:      v.Cfg.MemMiB,
		TrackDirtyPages: true, // pre-copy is impossible without KVM dirty logging
	}); err != nil {
		return err
	}
	if err := v.api.PutBootSource(fcapi.BootSource{
		KernelImagePath: v.Cfg.Kernel,
		BootArgs:        v.Cfg.bootArgs(),
	}); err != nil {
		return err
	}
	if err := v.api.PutDrive(fcapi.Drive{
		DriveID:      "rootfs",
		PathOnHost:   v.Cfg.Rootfs,
		IsRootDevice: true,
		IsReadOnly:   false,
	}); err != nil {
		return err
	}
	if err := v.api.PutNetworkInterface(fcapi.NetworkInterface{
		IfaceID:     "eth0",
		GuestMAC:    v.Cfg.GuestMAC,
		HostDevName: v.Cfg.TapDev,
	}); err != nil {
		return err
	}
	if err := v.api.Start(); err != nil {
		return err
	}
	if err := v.waitGuestReady(ctx); err != nil {
		return err
	}

	// Provision-time base snapshot: the one full-memory write in the VM's
	// life, taken while nobody is being served. Everything after this point
	// is tracked by the dirty bitmap.
	if err := v.api.Pause(); err != nil {
		return err
	}
	if err := v.api.CreateSnapshot(fcapi.SnapshotFull, v.baseStatePath(), v.baseMemPath(), 0); err != nil {
		return err
	}
	if err := v.api.Resume(); err != nil {
		return err
	}
	v.baseMem = v.baseMemPath()
	v.setState(StateRunning)
	return nil
}

// waitGuestReady polls guestd's status endpoint over the bridged network.
func (v *VM) waitGuestReady(ctx context.Context) error {
	guestIP, _, _ := splitCIDR(v.Cfg.GuestCIDR)
	url := fmt.Sprintf("http://%s:7780/status", guestIP)
	hc := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := hc.Get(url); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("guest %s did not become ready (no answer at %s)", v.Cfg.ID, url)
}

// kill terminates the VMM process, ignoring already-dead errors.
func (v *VM) kill() {
	if v.proc != nil {
		_ = v.proc.Kill()
	}
}

func splitCIDR(cidr string) (ip, prefix string, ok bool) {
	for i := range cidr {
		if cidr[i] == '/' {
			return cidr[:i], cidr[i+1:], true
		}
	}
	return cidr, "", false
}
