package nodeproto

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// EgressEntry is one Egress Rules or allowlist entry, `CIDR[:port]`
// (defaults.md "egress"). Port 0 means any protocol and port.
type EgressEntry struct {
	Prefix netip.Prefix
	Port   uint16
}

// ParseEgressEntry parses an IPv4 CIDR, or a single address meaning /32, with
// an optional port 1-65535.
func ParseEgressEntry(raw string) (EgressEntry, error) {
	addr, portRaw, hasPort := strings.Cut(raw, ":")
	var entry EgressEntry
	if hasPort {
		port, err := strconv.ParseUint(portRaw, 10, 16)
		if err != nil || port == 0 || portRaw[0] < '0' || portRaw[0] > '9' {
			return EgressEntry{}, fmt.Errorf("egress entry %q: port must be 1-65535", raw)
		}
		entry.Port = uint16(port)
	}
	if !strings.Contains(addr, "/") {
		addr += "/32"
	}
	prefix, err := netip.ParsePrefix(addr)
	if err != nil || !prefix.Addr().Is4() {
		return EgressEntry{}, fmt.Errorf("egress entry %q: want an IPv4 CIDR with an optional :port", raw)
	}
	entry.Prefix = prefix.Masked()
	return entry, nil
}
