//go:build linux

// migrated is the live-migration control plane, one instance per host.
package main

import (
	"flag"
	"log"

	"github.com/lgoyal6/fc-live-migration/internal/agent"
)

func main() {
	cfg := agent.Config{}
	flag.StringVar(&cfg.Name, "name", "host", "host name used in events/logs")
	flag.StringVar(&cfg.Listen, "listen", ":8500", "agent API listen address")
	flag.StringVar(&cfg.DataDir, "data", "/run/fcmig", "runtime dir (tmpfs) for sockets and snapshots")
	flag.StringVar(&cfg.FirecrackerBin, "firecracker", "/opt/fcmig/firecracker", "firecracker binary")
	flag.StringVar(&cfg.Kernel, "kernel", "/srv/vmstore/vmlinux", "guest kernel on shared storage")
	flag.StringVar(&cfg.Rootfs, "rootfs", "/srv/vmstore/rootfs.ext4", "guest rootfs on shared storage")
	flag.StringVar(&cfg.GARPIface, "garp-iface", "eth0", "interface to send gratuitous ARP on after receiving a VM")
	flag.BoolVar(&cfg.LiveRounds, "live-rounds", true,
		"use patched DiffLive snapshots for pause-free pre-copy (false = stock paused Diff rounds)")
	flag.Parse()

	if err := agent.New(cfg).ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
