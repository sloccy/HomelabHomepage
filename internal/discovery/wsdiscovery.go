package discovery

import (
	"context"
	"crypto/rand"
	"encoding/xml"
	"fmt"
	"log"
	"net"
	"strings"
	"time"
)

const wsdAddr = "239.255.255.250:3702"

// wsdProbeTemplate is the WS-Discovery Probe SOAP message.
// WS-Discovery is used by ONVIF cameras, printers, Windows devices, and some NAS firmware.
const wsdProbeTemplate = `<?xml version="1.0" encoding="UTF-8"?>` +
	`<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"` +
	` xmlns:a="http://schemas.xmlsoap.org/ws/2004/08/addressing"` +
	` xmlns:d="http://schemas.xmlsoap.org/ws/2005/04/discovery">` +
	`<s:Header>` +
	`<a:Action>http://schemas.xmlsoap.org/ws/2005/04/discovery/Probe</a:Action>` +
	`<a:MessageID>%s</a:MessageID>` +
	`<a:To>urn:schemas-xmlsoap-org:ws:2005:04:discovery</a:To>` +
	`</s:Header>` +
	`<s:Body><d:Probe><d:Types/></d:Probe></s:Body>` +
	`</s:Envelope>`

// wsdEnvelope is used to parse WS-Discovery ProbeMatch responses.
// Go's xml decoder matches on local names, so namespace prefixes are ignored.
type wsdEnvelope struct {
	Body struct {
		ProbeMatches struct {
			ProbeMatch []struct {
				XAddrs string `xml:"XAddrs"`
			} `xml:"ProbeMatch"`
		} `xml:"ProbeMatches"`
	} `xml:"Body"`
}

// discoverWSDiscovery sends a WS-Discovery Probe to 239.255.255.250:3702 and
// collects ProbeMatch responses. Extracts XAddrs (endpoint URLs) and returns
// them as openPort entries for HTTP probing. Used by ONVIF cameras, printers,
// Windows devices, and some NAS/storage firmware.
func discoverWSDiscovery(ctx context.Context, timeout time.Duration) []openPort {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		log.Printf("discovery: wsd: listen: %v", err)
		return nil
	}
	defer func() { _ = conn.Close() }()

	defer closeOnCancel(ctx, conn)()

	probe := fmt.Sprintf(wsdProbeTemplate, wsdMessageID())
	dst, _ := net.ResolveUDPAddr("udp4", wsdAddr)
	if _, err := conn.WriteTo([]byte(probe), dst); err != nil {
		log.Printf("discovery: wsd: send Probe: %v", err)
		return nil
	}
	_ = conn.SetReadDeadline(time.Now().Add(timeout))

	seen := make(map[string]bool)
	var ports []openPort

	buf := make([]byte, 65536)
	for {
		nbytes, _, err := conn.ReadFrom(buf)
		if err != nil {
			break
		}
		var env wsdEnvelope
		if err := xml.Unmarshal(buf[:nbytes], &env); err != nil {
			continue
		}
		for _, match := range env.Body.ProbeMatches.ProbeMatch {
			for _, addr := range strings.Fields(match.XAddrs) {
				p, ok := resolveURLToPort(addr)
				if !ok {
					continue
				}
				key := fmt.Sprintf("%s:%d", p.ip, p.port)
				if !seen[key] {
					seen[key] = true
					ports = append(ports, p)
				}
			}
		}
	}

	log.Printf("discovery: wsd: found %d services", len(ports))
	return ports
}

func wsdMessageID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("uuid:%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
