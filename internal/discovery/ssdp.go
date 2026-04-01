package discovery

import (
	"bufio"
	"bytes"
	"context"
	"log"
	"net"
	"net/textproto"
	"strings"
	"time"
)

const (
	ssdpAddr = "239.255.255.250:1900"
	ssdpMsg  = "M-SEARCH * HTTP/1.1\r\n" +
		"HOST: 239.255.255.250:1900\r\n" +
		"MAN: \"ssdp:discover\"\r\n" +
		"MX: 3\r\n" +
		"ST: ssdp:all\r\n" +
		"\r\n"
)

// discoverSSDP sends an SSDP M-SEARCH to 239.255.255.250:1900 and collects
// Location URLs from responses, returning the distinct IP:port pairs for HTTP
// probing. Fails silently if the host doesn't support UDP multicast.
func discoverSSDP(ctx context.Context, timeout time.Duration) []openPort {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		log.Printf("discovery: ssdp: listen: %v", err)
		return nil
	}
	defer func() { _ = conn.Close() }()

	defer closeOnCancel(ctx, conn)()

	dst, _ := net.ResolveUDPAddr("udp4", ssdpAddr)
	if _, err := conn.WriteTo([]byte(ssdpMsg), dst); err != nil {
		log.Printf("discovery: ssdp: send M-SEARCH: %v", err)
		return nil
	}
	_ = conn.SetReadDeadline(time.Now().Add(timeout))

	seen := make(map[string]bool)
	var ports []openPort

	buf := make([]byte, 4096)
	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			break // deadline, closed, or ctx cancel
		}
		loc := parseSSDPLocation(buf[:n])
		if loc == "" || seen[loc] {
			continue
		}
		seen[loc] = true

		p, ok := resolveURLToPort(ctx, loc)
		if !ok {
			continue
		}
		ports = append(ports, p)
	}

	log.Printf("discovery: ssdp: found %d unique services", len(ports))
	return ports
}

// parseSSDPLocation extracts the Location header from an SSDP M-SEARCH response.
// Only accepts HTTP 200 OK responses (not NOTIFY announcements).
func parseSSDPLocation(data []byte) string {
	r := textproto.NewReader(bufio.NewReader(bytes.NewReader(data)))
	line, err := r.ReadLine()
	if err != nil || !strings.Contains(line, "200 OK") {
		return ""
	}
	h, _ := r.ReadMIMEHeader()
	return h.Get("Location")
}
