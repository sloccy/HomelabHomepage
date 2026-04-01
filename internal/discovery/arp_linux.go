//go:build linux

package discovery

import (
	"context"
	"log"
	"net"
	"syscall"
	"time"
)

// arpSweep sends ARP requests for every IP in the list and returns the set of
// hosts that replied (i.e., live hosts on the same L2 segment). Requires
// CAP_NET_RAW; returns nil on permission error so the caller falls back to
// sweeping all IPs. timeout controls how long to wait for replies after all
// requests have been sent.
func arpSweep(ctx context.Context, ips []string, timeout time.Duration) map[string]bool {
	infos := buildIfaceInfos()
	if len(infos) == 0 {
		return nil
	}

	fd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW, int(arpHtons(syscall.ETH_P_ARP)))
	if err != nil {
		log.Printf("discovery: arp: raw socket unavailable (add CAP_NET_RAW for ARP pre-sweep): %v", err)
		return nil
	}
	defer func() { _ = syscall.Close(fd) }()

	// Cancel reads if ctx is done alongside the SO_RCVTIMEO deadline.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			syscall.Close(fd) //nolint:errcheck
		case <-done:
		}
	}()

	// Send an ARP request for each target IP that shares a subnet with a local interface.
	targets := make(map[string]bool)
	for _, ipStr := range ips {
		ip4 := net.ParseIP(ipStr).To4()
		if ip4 == nil {
			continue
		}
		src := findIfaceForIP(infos, ip4)
		if src == nil {
			continue // IP not reachable on any local subnet — skip
		}
		targets[ipStr] = true
		pkt := buildARPRequest(src.mac, src.ip, ip4)
		sa := &syscall.SockaddrLinklayer{
			Protocol: arpHtons(syscall.ETH_P_ARP),
			Ifindex:  src.index,
			Halen:    6,
		}
		// Broadcast destination MAC.
		sa.Addr[0], sa.Addr[1], sa.Addr[2], sa.Addr[3], sa.Addr[4], sa.Addr[5] = 0xff, 0xff, 0xff, 0xff, 0xff, 0xff
		_ = syscall.Sendto(fd, pkt, 0, sa)
	}

	if len(targets) == 0 {
		return nil
	}

	// Read ARP replies until timeout.
	tv := syscall.NsecToTimeval(timeout.Nanoseconds())
	_ = syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv)

	live := make(map[string]bool)
	buf := make([]byte, 60)
	for {
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		if err != nil || n < 42 {
			break
		}
		// Ethernet frame layout:
		//   [0:6]   dst MAC
		//   [6:12]  src MAC
		//   [12:14] EtherType  — must be 0x0806 (ARP)
		//   [14:16] HW type
		//   [16:18] proto type
		//   [18]    HW addr len
		//   [19]    proto addr len
		//   [20:22] opcode     — must be 0x0002 (reply)
		//   [22:28] sender MAC
		//   [28:32] sender IP  ← what we want
		if buf[12] != 0x08 || buf[13] != 0x06 {
			continue // not ARP
		}
		if buf[20] != 0x00 || buf[21] != 0x02 {
			continue // not ARP reply
		}
		senderIP := net.IP(buf[28:32]).String()
		if targets[senderIP] {
			live[senderIP] = true
		}
	}

	log.Printf("discovery: arp: %d/%d hosts responded", len(live), len(targets))
	return live
}

// ── helpers ───────────────────────────────────────────────────────────────────

type ifaceInfo struct {
	index int
	ip    net.IP
	mask  net.IPMask
	mac   net.HardwareAddr
}

func buildIfaceInfos() []ifaceInfo {
	ifaces, _ := net.Interfaces()
	var out []ifaceInfo
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 || len(iface.HardwareAddr) < 6 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil {
				continue
			}
			out = append(out, ifaceInfo{
				index: iface.Index,
				ip:    ip4,
				mask:  ipnet.Mask,
				mac:   iface.HardwareAddr,
			})
		}
	}
	return out
}

func findIfaceForIP(infos []ifaceInfo, target net.IP) *ifaceInfo {
	// Exact subnet match.
	for i := range infos {
		if infos[i].ip.Mask(infos[i].mask).Equal(target.Mask(infos[i].mask)) {
			return &infos[i]
		}
	}
	// Fallback: same /24 — handles the case where the interface mask is narrower
	// than the scan range (e.g. a /25 interface scanning an explicit /24 CIDR).
	mask24 := net.CIDRMask(24, 32)
	for i := range infos {
		if infos[i].ip.Mask(mask24).Equal(target.Mask(mask24)) {
			return &infos[i]
		}
	}
	return nil
}

// buildARPRequest crafts a raw Ethernet+ARP request frame (42 bytes).
func buildARPRequest(srcMAC net.HardwareAddr, srcIP, dstIP net.IP) []byte {
	pkt := make([]byte, 42)
	// Ethernet header
	pkt[0], pkt[1], pkt[2], pkt[3], pkt[4], pkt[5] = 0xff, 0xff, 0xff, 0xff, 0xff, 0xff // dst: broadcast
	copy(pkt[6:12], srcMAC)
	pkt[12], pkt[13] = 0x08, 0x06 // EtherType: ARP
	// ARP header
	pkt[14], pkt[15] = 0x00, 0x01 // HW type: Ethernet
	pkt[16], pkt[17] = 0x08, 0x00 // Proto type: IPv4
	pkt[18] = 6                   // HW addr length
	pkt[19] = 4                   // Proto addr length
	pkt[20], pkt[21] = 0x00, 0x01 // Opcode: request
	copy(pkt[22:28], srcMAC)
	copy(pkt[28:32], srcIP.To4())
	// Target MAC: zeros (already zero from make)
	copy(pkt[38:42], dstIP.To4())
	return pkt
}

// arpHtons converts a uint16 from host to network (big-endian) byte order.
func arpHtons(v uint16) uint16 { return v<<8 | v>>8 }
