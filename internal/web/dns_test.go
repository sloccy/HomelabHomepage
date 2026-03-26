package web

import "testing"

func TestPlanDNS(t *testing.T) {
	tests := []struct {
		name string
		old  dnsState
		want dnsState
		act  dnsAction
	}{
		// Direct → *
		{
			name: "direct to direct",
			old:  dnsState{DirectOnly: true},
			want: dnsState{DirectOnly: true},
			act:  dnsNoop,
		},
		{
			name: "direct to A record",
			old:  dnsState{DirectOnly: true},
			want: dnsState{Subdomain: "plex"},
			act:  dnsCreateA,
		},
		{
			name: "direct to tunnel",
			old:  dnsState{DirectOnly: true},
			want: dnsState{Subdomain: "plex", Tunnel: true},
			act:  dnsCreateTunnel,
		},

		// A record → *
		{
			name: "A record to direct",
			old:  dnsState{Subdomain: "plex", DNSRecordID: "rec1"},
			want: dnsState{DirectOnly: true},
			act:  dnsDeleteAll,
		},
		{
			name: "A record unchanged",
			old:  dnsState{Subdomain: "plex", DNSRecordID: "rec1"},
			want: dnsState{Subdomain: "plex"},
			act:  dnsNoop,
		},
		{
			name: "A record subdomain change",
			old:  dnsState{Subdomain: "old", DNSRecordID: "rec1"},
			want: dnsState{Subdomain: "new"},
			act:  dnsSwapA,
		},
		{
			name: "A record to tunnel",
			old:  dnsState{Subdomain: "plex", DNSRecordID: "rec1"},
			want: dnsState{Subdomain: "plex", Tunnel: true},
			act:  dnsAToTunnel,
		},

		// Tunnel → *
		{
			name: "tunnel to direct",
			old:  dnsState{Subdomain: "plex", Tunnel: true, TunnelRouteID: "plex.example.com", DNSRecordID: "cname1"},
			want: dnsState{DirectOnly: true},
			act:  dnsDeleteAll,
		},
		{
			name: "tunnel to A record",
			old:  dnsState{Subdomain: "plex", Tunnel: true, TunnelRouteID: "plex.example.com", DNSRecordID: "cname1"},
			want: dnsState{Subdomain: "plex"},
			act:  dnsTunnelToA,
		},
		{
			name: "tunnel unchanged",
			old:  dnsState{Subdomain: "plex", Tunnel: true, Target: "http://10.0.0.1:32400"},
			want: dnsState{Subdomain: "plex", Tunnel: true, Target: "http://10.0.0.1:32400"},
			act:  dnsNoop,
		},
		{
			name: "tunnel subdomain change",
			old:  dnsState{Subdomain: "old", Tunnel: true, Target: "http://10.0.0.1:32400"},
			want: dnsState{Subdomain: "new", Tunnel: true, Target: "http://10.0.0.1:32400"},
			act:  dnsReplaceTunnel,
		},
		{
			name: "tunnel target change",
			old:  dnsState{Subdomain: "plex", Tunnel: true, Target: "http://10.0.0.1:32400"},
			want: dnsState{Subdomain: "plex", Tunnel: true, Target: "http://10.0.0.2:32400"},
			act:  dnsReplaceTunnel,
		},
		{
			name: "tunnel subdomain and target change",
			old:  dnsState{Subdomain: "old", Tunnel: true, Target: "http://10.0.0.1:32400"},
			want: dnsState{Subdomain: "new", Tunnel: true, Target: "http://10.0.0.2:32400"},
			act:  dnsReplaceTunnel,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := planDNS(tc.old, tc.want)
			if got != tc.act {
				t.Errorf("planDNS(%+v, %+v) = %d, want %d", tc.old, tc.want, got, tc.act)
			}
		})
	}
}
