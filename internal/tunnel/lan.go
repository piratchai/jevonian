package tunnel

import (
	"fmt"
	"net"
	"strings"

	"github.com/xinyao27/jevonian/internal/config"
)

// LanPortOffset keeps the LAN surface clear of the tunnel's listen.port + 1.
// Mirrors LAN_PORT_OFFSET in src/config.ts.
const LanPortOffset = 2

// InterfaceAddr is one address on a network interface.
type InterfaceAddr struct {
	IP       net.IP
	Internal bool // loopback
}

// InterfaceLister lists interface addresses in OS order. Injectable for tests.
type InterfaceLister func() ([]InterfaceAddr, error)

// SystemInterfaces reads the host's interfaces.
func SystemInterfaces() ([]InterfaceAddr, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []InterfaceAddr
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil {
				continue
			}
			out = append(out, InterfaceAddr{
				IP:       ip,
				Internal: iface.Flags&net.FlagLoopback != 0 || ip.IsLoopback(),
			})
		}
	}
	return out, nil
}

// LanIPv4Addresses are the IPv4 addresses a peer on the same network can reach
// this machine at.
//
// Only non-internal IPv4 addresses qualify: loopback is not reachable from
// another host, and link-local IPv6 needs a zone id a user would not know to
// type. Addresses keep the OS order, deduplicated, so the first entry is a
// stable "most likely" address to show.
func LanIPv4Addresses(list InterfaceLister) []string {
	if list == nil {
		list = SystemInterfaces
	}
	addrs, err := list()
	if err != nil {
		return nil
	}
	var found []string
	seen := map[string]bool{}
	for _, a := range addrs {
		v4 := a.IP.To4()
		if v4 == nil || a.Internal {
			continue
		}
		s := v4.String()
		if seen[s] {
			continue
		}
		seen[s] = true
		found = append(found, s)
	}
	return found
}

// IsReachableFromLan reports whether a bind address accepts connections from
// other machines.
func IsReachableFromLan(host string) bool {
	switch strings.TrimSpace(host) {
	case "0.0.0.0", "::", "*":
		return true
	}
	return false
}

// LanPort is the port the LAN surface listens on, defaulting clear of the
// tunnel's listen.port + 1.
func LanPort(cfg *config.Config) int {
	if cfg.Lan.Port != nil {
		return *cfg.Lan.Port
	}
	return cfg.Listen.Port + LanPortOffset
}

// LanBindHost is the bind host for the LAN surface; every interface unless a
// specific one was chosen.
func LanBindHost(lan config.LanConfig) string {
	if lan.Host != "" {
		return lan.Host
	}
	return "0.0.0.0"
}

// LanBaseURLs are the base URLs a peer should use to reach this instance's /v1
// surface. When the bind is a specific address only that address is offered;
// when it is all interfaces, every LAN IPv4 is. Empty when the machine has no
// non-loopback interface, so the console can say so rather than print a URL
// that cannot work.
func LanBaseURLs(cfg *config.Config, addresses []string) []string {
	port := LanPort(cfg)
	host := LanBindHost(cfg.Lan)
	hosts := []string{host}
	if IsReachableFromLan(host) {
		hosts = addresses
	}
	out := make([]string, 0, len(hosts))
	for _, address := range hosts {
		out = append(out, fmt.Sprintf("http://%s/v1", net.JoinHostPort(address, fmt.Sprint(port))))
	}
	return out
}
