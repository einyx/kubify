package agentfw

import (
	"fmt"
	"net"
	"sync"
	"time"
)

type dnsEntry struct {
	ip      string
	expires time.Time
}

// DNSCache detects DNS rebinding by caching resolved IPs and flagging changes.
type DNSCache struct {
	mu sync.Mutex
	m  map[string]dnsEntry
}

func NewDNSCache() *DNSCache { return &DNSCache{m: make(map[string]dnsEntry)} }

// Check resolves host and returns an error if the IP is private or has changed
// since the last resolution (DNS rebinding signal).
func (c *DNSCache) Check(host string) error {
	// strip port
	h := host
	if p := lastColon(host); p > 0 {
		h = host[:p]
	}

	addrs, err := net.LookupHost(h)
	if err != nil {
		return fmt.Errorf("agentfw: dns lookup failed for %s: %w", h, err)
	}
	ip := addrs[0]

	// Block if resolved IP is private/loopback regardless of hostname.
	if IsPrivateHost(ip) {
		return fmt.Errorf("agentfw: DNS rebinding blocked — %s resolved to private IP %s", h, ip)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if prev, ok := c.m[h]; ok && now.Before(prev.expires) {
		if prev.ip != ip {
			return fmt.Errorf("agentfw: DNS rebinding detected — %s changed from %s to %s", h, prev.ip, ip)
		}
	}
	c.m[h] = dnsEntry{ip: ip, expires: now.Add(60 * time.Second)}
	return nil
}

func lastColon(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == ':' {
			return i
		}
	}
	return -1
}
