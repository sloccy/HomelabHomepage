package discovery

import (
	"context"
	"log"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/miekg/dns"
)

var mdnsGroupAddr = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}

// mdnsServicesMeta is the DNS-SD meta-query that returns all advertised service
// types on the local network. Responses are PTR records pointing to type names.
const mdnsServicesMeta = "_services._dns-sd._udp.local."

// mdnsKnownServices is a seed set of service types queried immediately alongside
// the meta-query so common services are found even if the meta-query is slow.
var mdnsKnownServices = []string{
	"_http._tcp.local.",
	"_https._tcp.local.",
	"_plex._tcp.local.",
	"_home-assistant._tcp.local.",
	"_esphomelib._tcp.local.",
	"_workstation._tcp.local.",
	"_googlecast._tcp.local.",
	"_companion-link._tcp.local.",
}

// discoverMDNS sends mDNS PTR queries for known service types and correlates
// the SRV + A records from responses into (IP, port) pairs for HTTP probing.
// It also sends a DNS-SD meta-query (_services._dns-sd._udp.local.) to discover
// service types not in the seed list, querying each one dynamically as they arrive.
// Returns nil and logs a message if multicast is unavailable (common in Docker
// bridge networks — use --network=host or a macvlan network for mDNS support).
func discoverMDNS(ctx context.Context, timeout time.Duration) []openPort {
	conn, err := net.ListenMulticastUDP("udp4", nil, mdnsGroupAddr)
	if err != nil {
		log.Printf("discovery: mdns: cannot join multicast group (use --network=host for mDNS): %v", err)
		return nil
	}
	defer conn.Close()

	// Cancel the blocking read when ctx is done.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()

	_ = conn.SetReadDeadline(time.Now().Add(timeout))

	// Track queried service types to avoid duplicate queries.
	queriedTypes := make(map[string]bool)
	sendQuery := func(serviceType string) {
		if queriedTypes[serviceType] {
			return
		}
		queriedTypes[serviceType] = true
		m := new(dns.Msg)
		m.SetQuestion(serviceType, dns.TypePTR)
		m.RecursionDesired = false
		b, err := m.Pack()
		if err != nil {
			return
		}
		_, _ = conn.WriteTo(b, mdnsGroupAddr)
	}

	// Meta-query: ask the network "what service types are advertised here?"
	// Responses are PTR records whose Ptr field names the service type.
	sendQuery(mdnsServicesMeta)
	// Seed queries for well-known types to avoid waiting for meta-query responses.
	for _, svc := range mdnsKnownServices {
		sendQuery(svc)
	}

	// Collect DNS records across all responses within the timeout window.
	type srvInfo struct {
		host string
		port uint16
	}
	srvMap := make(map[string]srvInfo) // instance FQDN → SRV
	aMap := make(map[string]string)    // hostname FQDN → IPv4

	buf := make([]byte, 65536)
	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			break // deadline, closed, or ctx cancel
		}
		var msg dns.Msg
		if err := msg.Unpack(buf[:n]); err != nil {
			continue
		}
		if !msg.Response {
			continue // skip our own outbound queries (multicast loopback)
		}
		for _, rr := range append(append(msg.Answer, msg.Ns...), msg.Extra...) {
			switch v := rr.(type) {
			case *dns.PTR:
				// Meta-query response: _services._dns-sd._udp.local. PTR _foo._tcp.local.
				// Dynamically query the newly discovered service type.
				if strings.ToLower(v.Hdr.Name) == mdnsServicesMeta {
					sendQuery(strings.ToLower(v.Ptr))
				}
			case *dns.SRV:
				srvMap[strings.ToLower(v.Hdr.Name)] = srvInfo{
					host: strings.ToLower(v.Target),
					port: v.Port,
				}
			case *dns.A:
				aMap[strings.ToLower(v.Hdr.Name)] = v.A.String()
			}
		}
	}

	// Correlate SRV → hostname → IP to produce open port list.
	seen := make(map[string]bool)
	var ports []openPort
	for _, srv := range srvMap {
		ip, ok := aMap[srv.host]
		if !ok {
			// Try the system resolver (works for .local names with avahi/nss-mdns).
			host := strings.TrimSuffix(srv.host, ".")
			addrs, err := net.LookupHost(host)
			if err != nil || len(addrs) == 0 {
				continue
			}
			ip = addrs[0]
		}
		key := ip + ":" + strconv.Itoa(int(srv.port))
		if !seen[key] {
			seen[key] = true
			ports = append(ports, openPort{ip: ip, port: int(srv.port)})
		}
	}

	log.Printf("discovery: mdns: found %d services across %d service types", len(ports), len(queriedTypes))
	return ports
}
