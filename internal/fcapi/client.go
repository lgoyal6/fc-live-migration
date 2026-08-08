// Package fcapi is a minimal client for the Firecracker API served over a
// unix domain socket. It covers exactly the endpoints this project uses; see
// the Firecracker OpenAPI spec for the full surface.
package fcapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

// SnapshotType selects the memory dump strategy of PUT /snapshot/create.
//
// Full and Diff are upstream Firecracker. DiffLive is added by
// patches/0001-*.patch: identical to Diff except that it skips the vCPU/device
// state save (the only part that requires a paused VM) and does not reset the
// dirty-page bookkeeping, so it can run while vCPUs execute — the primitive
// that makes pre-copy rounds pause-free.
type SnapshotType string

const (
	SnapshotFull     SnapshotType = "Full"
	SnapshotDiff     SnapshotType = "Diff"
	SnapshotDiffLive SnapshotType = "DiffLive"
)

// Client talks to one Firecracker process.
type Client struct {
	sock string
	hc   *http.Client
}

func New(sock string) *Client {
	return &Client{
		sock: sock,
		hc: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", sock)
				},
				// One kept-alive connection: the final migration round must
				// not pay for a fresh socket handshake inside the blackout.
				MaxIdleConns:    1,
				IdleConnTimeout: 90 * time.Second,
			},
			Timeout: 30 * time.Second,
		},
	}
}

// apiError carries Firecracker's fault_message.
type apiError struct {
	status int
	fault  string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("firecracker API: HTTP %d: %s", e.status, e.fault)
}

func (c *Client) do(method, path string, body any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://localhost"+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var fault struct {
			FaultMessage string `json:"fault_message"`
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if json.Unmarshal(raw, &fault) != nil || fault.FaultMessage == "" {
			fault.FaultMessage = string(raw)
		}
		return &apiError{status: resp.StatusCode, fault: fault.FaultMessage}
	}
	// Drain so the keep-alive connection is reusable.
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// WaitReady polls until the API socket accepts requests.
func (c *Client) WaitReady(ctx context.Context) error {
	for {
		if _, err := os.Stat(c.sock); err == nil {
			if err := c.do(http.MethodGet, "/", nil); err == nil {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("firecracker API socket %s not ready: %w", c.sock, ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// MachineConfig corresponds to PUT /machine-config.
type MachineConfig struct {
	VcpuCount       int  `json:"vcpu_count"`
	MemSizeMiB      int  `json:"mem_size_mib"`
	TrackDirtyPages bool `json:"track_dirty_pages"`
}

func (c *Client) PutMachineConfig(cfg MachineConfig) error {
	return c.do(http.MethodPut, "/machine-config", cfg)
}

// BootSource corresponds to PUT /boot-source.
type BootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args,omitempty"`
}

func (c *Client) PutBootSource(b BootSource) error {
	return c.do(http.MethodPut, "/boot-source", b)
}

// Drive corresponds to PUT /drives/{id}.
type Drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

func (c *Client) PutDrive(d Drive) error {
	return c.do(http.MethodPut, "/drives/"+d.DriveID, d)
}

// NetworkInterface corresponds to PUT /network-interfaces/{id}.
type NetworkInterface struct {
	IfaceID     string `json:"iface_id"`
	GuestMAC    string `json:"guest_mac"`
	HostDevName string `json:"host_dev_name"`
}

func (c *Client) PutNetworkInterface(n NetworkInterface) error {
	return c.do(http.MethodPut, "/network-interfaces/"+n.IfaceID, n)
}

// Start issues the InstanceStart action.
func (c *Client) Start() error {
	return c.do(http.MethodPut, "/actions", map[string]string{"action_type": "InstanceStart"})
}

// Pause stops all vCPUs. Sub-millisecond: a signal + channel ack per vCPU.
func (c *Client) Pause() error {
	return c.do(http.MethodPatch, "/vm", map[string]string{"state": "Paused"})
}

// Resume restarts all vCPUs.
func (c *Client) Resume() error {
	return c.do(http.MethodPatch, "/vm", map[string]string{"state": "Resumed"})
}

// CreateSnapshot corresponds to PUT /snapshot/create. For DiffLive the
// statePath is still required by the API schema but no state file is
// written; liveMaxBytes (0 = unlimited) caps how much memory one DiffLive
// call may dump, bounding its occupancy of the VMM event loop.
func (c *Client) CreateSnapshot(t SnapshotType, statePath, memPath string, liveMaxBytes int64) error {
	body := map[string]any{
		"snapshot_type": string(t),
		"snapshot_path": statePath,
		"mem_file_path": memPath,
	}
	if liveMaxBytes > 0 {
		body["live_max_bytes"] = liveMaxBytes
	}
	return c.do(http.MethodPut, "/snapshot/create", body)
}

// LoadSnapshot corresponds to PUT /snapshot/load with a file-backed memory
// source. trackDirty must be true for the restored VM to be migratable again
// (our ping-pong benchmark relies on this); resume avoids a separate
// PATCH /vm round-trip inside the blackout window.
func (c *Client) LoadSnapshot(statePath, memPath string, trackDirty, resume bool) error {
	return c.do(http.MethodPut, "/snapshot/load", map[string]any{
		"snapshot_path": statePath,
		"mem_backend": map[string]string{
			"backend_type": "File",
			"backend_path": memPath,
		},
		"track_dirty_pages": trackDirty,
		"resume_vm":         resume,
	})
}
