// Package safenet provides network security primitives for agent tool use.
// Primarily: SSRF prevention for URL fetching tools.
package safenet

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ValidateFetchURL checks a URL for SSRF risks before fetching.
// Blocks: non-http(s) schemes, private/loopback IPs, metadata endpoints,
// DNS names that resolve to private IPs.
func ValidateFetchURL(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	// Scheme check
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("blocked scheme %q: only http/https allowed", parsed.Scheme)
	}

	host := parsed.Hostname()
	if host == "" {
		return fmt.Errorf("empty hostname")
	}

	// Hostname blacklist (cloud metadata endpoints)
	lowerHost := strings.ToLower(host)
	blockedHosts := []string{
		"metadata.google.internal",
		"metadata.goog",
		"169.254.169.254", // AWS/GCP metadata
		"100.100.100.200", // Alibaba Cloud metadata
		"fd00:ec2::254",   // AWS IMDSv2 IPv6
	}
	for _, blocked := range blockedHosts {
		if lowerHost == blocked {
			return fmt.Errorf("blocked host %q: cloud metadata endpoint", host)
		}
	}
	blockedSuffixes := []string{".local", ".internal", ".localhost"}
	for _, suffix := range blockedSuffixes {
		if strings.HasSuffix(lowerHost, suffix) {
			return fmt.Errorf("blocked host %q: internal domain", host)
		}
	}

	// IP literal check
	if ip := net.ParseIP(host); ip != nil {
		if err := checkIP(ip); err != nil {
			return err
		}
		return nil
	}

	// DNS resolution check — resolve and validate all IPs
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("DNS lookup failed for %q: %w", host, err)
	}
	for _, ip := range ips {
		if err := checkIP(ip); err != nil {
			return fmt.Errorf("DNS %q resolved to blocked IP: %w", host, err)
		}
	}

	return nil
}

// checkIP validates a single IP address against private/loopback/link-local ranges.
func checkIP(ip net.IP) error {
	if ip.IsLoopback() {
		return fmt.Errorf("blocked IP %s: loopback", ip)
	}
	if ip.IsPrivate() {
		return fmt.Errorf("blocked IP %s: private network", ip)
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return fmt.Errorf("blocked IP %s: link-local", ip)
	}
	if ip.IsUnspecified() {
		return fmt.Errorf("blocked IP %s: unspecified", ip)
	}
	// CGNAT range: 100.64.0.0/10
	if ip4 := ip.To4(); ip4 != nil && ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
		return fmt.Errorf("blocked IP %s: CGNAT range", ip)
	}
	return nil
}
