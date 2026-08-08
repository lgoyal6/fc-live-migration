//go:build linux

package agent

import (
	"encoding/binary"
	"fmt"
	"net"
	"time"

	"golang.org/x/sys/unix"
)

// sendGARP broadcasts gratuitous ARP for (mac, ip) out of iface — the
// vMotion-style network handover. The frame's source MAC is the *guest's*,
// so every learning bridge on the path moves the guest's FDB entry to the
// port facing this host, and in-flight TCP connections simply start arriving
// here. Clients' ARP caches need no update (the IP→MAC mapping is
// unchanged); only switch forwarding tables do.
//
// We send both flavors seen in the wild (request with spa==tpa, and reply),
// three times each — GARP is fire-and-forget, and repetition is the standard
// hedge against a lost frame.
func sendGARP(iface, mac, ip string) error {
	hwAddr, err := net.ParseMAC(mac)
	if err != nil {
		return fmt.Errorf("parse guest MAC: %w", err)
	}
	ipAddr := net.ParseIP(ip).To4()
	if ipAddr == nil {
		return fmt.Errorf("guest IP %q is not IPv4", ip)
	}
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return fmt.Errorf("GARP interface %s: %w", iface, err)
	}

	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, 0)
	if err != nil {
		return fmt.Errorf("AF_PACKET socket: %w", err)
	}
	defer unix.Close(fd)

	addr := &unix.SockaddrLinklayer{
		Protocol: htons(unix.ETH_P_ARP),
		Ifindex:  ifi.Index,
		Halen:    6,
	}
	copy(addr.Addr[:], hwAddr)

	// The first frame is the one on the blackout critical path — fire the
	// whole burst back-to-back and keep only a tiny settle gap between
	// repeats (repetition hedges against a lost frame, nothing more).
	for i := 0; i < 3; i++ {
		for _, op := range []uint16{1, 2} { // ARP request, then reply
			if err := unix.Sendto(fd, buildGARPFrame(hwAddr, ipAddr, op), 0, addr); err != nil {
				return fmt.Errorf("send GARP: %w", err)
			}
		}
		time.Sleep(200 * time.Microsecond)
	}
	return nil
}

// buildGARPFrame assembles Ethernet + ARP with sender==target==(mac, ip).
func buildGARPFrame(mac net.HardwareAddr, ip net.IP, op uint16) []byte {
	b := make([]byte, 42)
	// Ethernet header.
	copy(b[0:6], net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	copy(b[6:12], mac)
	binary.BigEndian.PutUint16(b[12:14], unix.ETH_P_ARP)
	// ARP payload.
	binary.BigEndian.PutUint16(b[14:16], 1)      // htype: Ethernet
	binary.BigEndian.PutUint16(b[16:18], 0x0800) // ptype: IPv4
	b[18], b[19] = 6, 4                          // hlen, plen
	binary.BigEndian.PutUint16(b[20:22], op)
	copy(b[22:28], mac) // sender MAC
	copy(b[28:32], ip)  // sender IP
	// target MAC left zero (request) — harmless for reply-flavor GARP too
	copy(b[38:42], ip) // target IP == sender IP: "this is gratuitous"
	return b
}

func htons(v uint16) uint16 {
	return v<<8 | v>>8
}
