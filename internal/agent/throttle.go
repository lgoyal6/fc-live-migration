//go:build linux

package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Throttler implements auto-converge: when a guest dirties memory faster
// than pre-copy can ship it, migration would never converge, so we slow the
// guest until it does — the same trade QEMU makes by injecting vCPU sleeps.
//
// Mechanism: a SIGSTOP/SIGCONT duty cycle applied to the Firecracker
// process's vCPU threads only (found by their "fc_vcpu N" comm names).
// Targeting the threads rather than the process matters twice over: the VMM
// event loop keeps servicing virtio (the guest stays reachable, just slow),
// and our own snapshot API calls — the drain itself — keep executing at
// full speed. Level 0 = no throttle; levels 1..5 leave the guest roughly
// 60/35/20/10/5% of its CPU time.
const maxThrottleLevel = 5

var levelRunPct = [maxThrottleLevel + 1]int{100, 60, 35, 20, 10, 5}

type Throttler struct {
	mu      sync.Mutex
	pids    map[string]int // vmID → firecracker pid
	cyclers map[string]*dutyCycler
}

func NewThrottler() *Throttler {
	return &Throttler{pids: map[string]int{}, cyclers: map[string]*dutyCycler{}}
}

// Register binds a VM ID to its Firecracker pid. vCPU threads are resolved
// lazily at SetLevel time — they don't exist until the guest boots.
func (t *Throttler) Register(vmID string, pid int) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pids[vmID] = pid
	return nil
}

// SetLevel applies throttle level 0..maxThrottleLevel to a VM's vCPUs.
func (t *Throttler) SetLevel(vmID string, level int) error {
	if level < 0 || level > maxThrottleLevel {
		return fmt.Errorf("throttle level %d out of range", level)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	pid, ok := t.pids[vmID]
	if !ok {
		return fmt.Errorf("unknown VM %q", vmID)
	}

	if c := t.cyclers[vmID]; c != nil {
		c.stop()
		delete(t.cyclers, vmID)
	}
	if level == 0 {
		return nil
	}
	tids, err := vcpuThreads(pid)
	if err != nil {
		return err
	}
	if len(tids) == 0 {
		return fmt.Errorf("no vCPU threads found for pid %d", pid)
	}
	t.cyclers[vmID] = newDutyCycler(pid, tids, levelRunPct[level])
	return nil
}

// vcpuThreads finds the TIDs of a Firecracker process's vCPU threads.
func vcpuThreads(pid int) ([]int, error) {
	taskDir := fmt.Sprintf("/proc/%d/task", pid)
	entries, err := os.ReadDir(taskDir)
	if err != nil {
		return nil, fmt.Errorf("list threads of %d: %w", pid, err)
	}
	var tids []int
	for _, e := range entries {
		comm, err := os.ReadFile(filepath.Join(taskDir, e.Name(), "comm"))
		if err != nil {
			continue // thread exited mid-scan
		}
		if strings.HasPrefix(string(comm), "fc_vcpu") {
			if tid, err := strconv.Atoi(e.Name()); err == nil {
				tids = append(tids, tid)
			}
		}
	}
	return tids, nil
}

// dutyCycler stops and resumes a set of threads over a 20ms period.
type dutyCycler struct {
	pid  int
	tids []int
	done chan struct{}
}

func newDutyCycler(pid int, tids []int, runPct int) *dutyCycler {
	c := &dutyCycler{pid: pid, tids: tids, done: make(chan struct{})}
	go c.run(runPct)
	return c
}

func (c *dutyCycler) signalAll(sig unix.Signal) {
	for _, tid := range c.tids {
		// Thread-directed signal; ESRCH just means the VM went away.
		_ = unix.Tgkill(c.pid, tid, sig)
	}
}

func (c *dutyCycler) run(runPct int) {
	const period = 20 * time.Millisecond
	runFor := period * time.Duration(runPct) / 100
	for {
		select {
		case <-c.done:
			c.signalAll(unix.SIGCONT)
			return
		case <-time.After(runFor):
		}
		c.signalAll(unix.SIGSTOP)
		select {
		case <-c.done:
			c.signalAll(unix.SIGCONT)
			return
		case <-time.After(period - runFor):
		}
		c.signalAll(unix.SIGCONT)
	}
}

func (c *dutyCycler) stop() {
	close(c.done)
}
