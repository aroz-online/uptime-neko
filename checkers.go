package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	probing "github.com/prometheus-community/pro-bing"
)

/*
	checkers.go

	Each checker runs one probe and returns a CheckResult. They never panic and
	always honor the supplied timeout.
*/

// CheckResult is the outcome of a single probe
type CheckResult struct {
	Up         bool
	Latency    float64 // milliseconds
	Msg        string
	CertExpiry int64 // unix ms, 0 if not applicable
}

// runCheck dispatches to the right checker for a monitor
func runCheck(m *Monitor) CheckResult {
	timeout := time.Duration(m.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	switch m.Type {
	case MonitorHTTP:
		return checkHTTP(m, timeout)
	case MonitorTCP:
		return checkTCP(m, timeout)
	case MonitorPing:
		return checkPing(m, timeout)
	case MonitorDNS:
		return checkDNS(m, timeout)
	case MonitorSSL:
		return checkSSL(m, timeout)
	default:
		return CheckResult{Up: false, Msg: "unknown monitor type: " + string(m.Type)}
	}
}

func checkHTTP(m *Monitor, timeout time.Duration) CheckResult {
	target := m.Target
	if !strings.Contains(target, "://") {
		target = "http://" + target
	}
	method := m.Method
	if method == "" {
		method = http.MethodGet
	}

	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: m.IgnoreTLS},
			DisableKeepAlives: true,
		},
		// Follow redirects by default (Go default), keyword check on final body.
	}

	start := time.Now()
	req, err := http.NewRequest(method, target, nil)
	if err != nil {
		return CheckResult{Up: false, Msg: "bad request: " + err.Error()}
	}
	req.Header.Set("User-Agent", "Uptime-Neko/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return CheckResult{Up: false, Latency: msSince(start), Msg: err.Error()}
	}
	defer resp.Body.Close()

	var certExpiry int64
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		certExpiry = resp.TLS.PeerCertificates[0].NotAfter.UnixMilli()
	}

	// Read body only if we need keyword matching (cap to 1MB)
	bodyContainsKeyword := false
	if m.Keyword != "" {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		bodyContainsKeyword = strings.Contains(string(body), m.Keyword)
	} else {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	}
	latency := msSince(start)

	// Status code check
	statusOK := false
	if m.ExpectedStatus > 0 {
		statusOK = resp.StatusCode == m.ExpectedStatus
	} else {
		statusOK = resp.StatusCode >= 200 && resp.StatusCode < 400
	}
	if !statusOK {
		return CheckResult{Up: false, Latency: latency, Msg: fmt.Sprintf("HTTP %d", resp.StatusCode), CertExpiry: certExpiry}
	}

	// Keyword check
	if m.Keyword != "" {
		matched := bodyContainsKeyword
		if m.KeywordInvert {
			matched = !matched
		}
		if !matched {
			word := "missing"
			if m.KeywordInvert {
				word = "present"
			}
			return CheckResult{Up: false, Latency: latency, Msg: fmt.Sprintf("keyword %s: %q", word, m.Keyword), CertExpiry: certExpiry}
		}
	}

	return CheckResult{Up: true, Latency: latency, Msg: fmt.Sprintf("HTTP %d", resp.StatusCode), CertExpiry: certExpiry}
}

func checkTCP(m *Monitor, timeout time.Duration) CheckResult {
	addr := m.Target
	if m.Port > 0 && !strings.Contains(addr, ":") {
		addr = net.JoinHostPort(addr, strconv.Itoa(m.Port))
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return CheckResult{Up: false, Msg: "invalid host:port: " + addr}
	}
	start := time.Now()
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return CheckResult{Up: false, Latency: msSince(start), Msg: err.Error()}
	}
	conn.Close()
	return CheckResult{Up: true, Latency: msSince(start), Msg: "connected"}
}

func checkPing(m *Monitor, timeout time.Duration) CheckResult {
	host := m.Target
	// strip scheme/port if user pasted a URL
	if strings.Contains(host, "://") {
		if u, err := url.Parse(host); err == nil && u.Hostname() != "" {
			host = u.Hostname()
		}
	}
	host = strings.TrimSpace(host)

	pinger, err := probing.NewPinger(host)
	if err != nil {
		return CheckResult{Up: false, Msg: "resolve failed: " + err.Error()}
	}
	// On Windows, privileged (raw ICMP) mode is required and works for admins.
	// On Linux it may need cap_net_raw; UDP mode is the unprivileged fallback.
	pinger.SetPrivileged(true)
	pinger.Count = 1
	pinger.Timeout = timeout

	if err := pinger.Run(); err != nil {
		// Retry once in unprivileged UDP mode (Linux without raw socket caps)
		pinger.SetPrivileged(false)
		if err2 := pinger.Run(); err2 != nil {
			return CheckResult{Up: false, Msg: err.Error()}
		}
	}
	stats := pinger.Statistics()
	if stats.PacketsRecv == 0 {
		return CheckResult{Up: false, Msg: "100% packet loss"}
	}
	return CheckResult{Up: true, Latency: float64(stats.AvgRtt.Microseconds()) / 1000.0, Msg: "reply from " + host}
}

func checkDNS(m *Monitor, timeout time.Duration) CheckResult {
	host := strings.TrimSpace(m.Target)
	rtype := strings.ToUpper(m.DNSRecordType)
	if rtype == "" {
		rtype = "A"
	}

	resolver := net.DefaultResolver
	if m.DNSResolver != "" {
		server := m.DNSResolver
		if !strings.Contains(server, ":") {
			server += ":53"
		}
		resolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				d := net.Dialer{Timeout: timeout}
				return d.DialContext(ctx, network, server)
			},
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	start := time.Now()
	var results []string
	var err error
	switch rtype {
	case "A", "AAAA":
		var ips []net.IP
		ips, err = resolver.LookupIP(ctx, ipNetwork(rtype), host)
		for _, ip := range ips {
			results = append(results, ip.String())
		}
	case "CNAME":
		var cname string
		cname, err = resolver.LookupCNAME(ctx, host)
		if cname != "" {
			results = append(results, cname)
		}
	case "TXT":
		results, err = resolver.LookupTXT(ctx, host)
	case "NS":
		var ns []*net.NS
		ns, err = resolver.LookupNS(ctx, host)
		for _, n := range ns {
			results = append(results, n.Host)
		}
	case "MX":
		var mx []*net.MX
		mx, err = resolver.LookupMX(ctx, host)
		for _, x := range mx {
			results = append(results, x.Host)
		}
	default:
		return CheckResult{Up: false, Msg: "unsupported DNS record type: " + rtype}
	}
	latency := msSince(start)

	if err != nil {
		return CheckResult{Up: false, Latency: latency, Msg: err.Error()}
	}
	if len(results) == 0 {
		return CheckResult{Up: false, Latency: latency, Msg: "no records"}
	}
	if m.DNSExpected != "" {
		found := false
		for _, r := range results {
			if strings.Contains(r, m.DNSExpected) {
				found = true
				break
			}
		}
		if !found {
			return CheckResult{Up: false, Latency: latency, Msg: "expected value not found: " + m.DNSExpected}
		}
	}
	return CheckResult{Up: true, Latency: latency, Msg: strings.Join(results, ", ")}
}

func checkSSL(m *Monitor, timeout time.Duration) CheckResult {
	addr := m.Target
	// allow https URLs
	if strings.Contains(addr, "://") {
		if u, err := url.Parse(addr); err == nil {
			addr = u.Host
		}
	}
	if !strings.Contains(addr, ":") {
		port := m.Port
		if port == 0 {
			port = 443
		}
		addr = net.JoinHostPort(addr, strconv.Itoa(port))
	}
	host, _, _ := net.SplitHostPort(addr)

	start := time.Now()
	dialer := &net.Dialer{Timeout: timeout}
	conn, err := tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: host})
	if err != nil {
		return CheckResult{Up: false, Latency: msSince(start), Msg: err.Error()}
	}
	defer conn.Close()
	latency := msSince(start)

	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return CheckResult{Up: false, Latency: latency, Msg: "no certificate presented"}
	}
	cert := certs[0]
	expiry := cert.NotAfter
	days := int(time.Until(expiry).Hours() / 24)
	if time.Now().After(expiry) {
		return CheckResult{Up: false, Latency: latency, Msg: "certificate expired", CertExpiry: expiry.UnixMilli()}
	}
	return CheckResult{
		Up:         true,
		Latency:    latency,
		Msg:        fmt.Sprintf("valid, expires in %d days", days),
		CertExpiry: expiry.UnixMilli(),
	}
}

func ipNetwork(rtype string) string {
	if rtype == "AAAA" {
		return "ip6"
	}
	return "ip4"
}

func msSince(t time.Time) float64 {
	return float64(time.Since(t).Microseconds()) / 1000.0
}
