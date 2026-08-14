package config

import (
	"net"
	"sort"
	"strings"

	"github.com/seitzbg/heliograph/internal/model"
)

// maxDisplayIPs caps how many resolved addresses a title shows, so a host with many A/AAAA
// records can't grow the title unbounded.
const maxDisplayIPs = 3

// DisplayIP returns the address to show in a monitor's title, in priority order:
//
//  1. the pinned m.IP (display-only; no DNS),
//  2. m.Host if it is already a literal IP,
//  3. the addresses lookup returns for the hostname — deduped, sorted, capped at
//     maxDisplayIPs, and comma-joined.
//
// It returns "" when there is nothing to show (no pin, a hostname host, and either a nil
// lookup or a lookup that resolved nothing). lookup is injected so the priority logic is
// testable without real DNS; callers pass nil to skip resolution.
func DisplayIP(m model.Monitor, lookup func(host string) []string) string {
	if m.IP != "" {
		return m.IP
	}
	if net.ParseIP(m.Host) != nil {
		return m.Host
	}
	if lookup == nil {
		return ""
	}
	seen := map[string]bool{}
	var ips []string
	for _, ip := range lookup(m.Host) {
		if ip != "" && !seen[ip] {
			seen[ip] = true
			ips = append(ips, ip)
		}
	}
	sort.Strings(ips)
	if len(ips) > maxDisplayIPs {
		ips = ips[:maxDisplayIPs]
	}
	return strings.Join(ips, ", ")
}
