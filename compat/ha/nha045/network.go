package nha045

import (
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
)

type NetworkEvidence struct {
	Interfaces   []string `json:"interfaces"`
	IPv4Routes   string   `json:"ipv4_routes"`
	IPv6Routes   string   `json:"ipv6_routes"`
	LoopbackOnly bool     `json:"loopback_only"`
}

// InspectNetwork is deliberately fail-closed on a normal host. The caller must
// additionally inspect the actual Docker envelope before starting this process.
func InspectNetwork() (NetworkEvidence, error) {
	evidence := NetworkEvidence{Interfaces: []string{}}
	if runtime.GOOS != "linux" {
		return evidence, fail(InfrastructureUnavailable, "live qualification requires the isolated Linux namespace")
	}
	interfaces, e := net.Interfaces()
	if e != nil {
		return evidence, e
	}
	if len(interfaces) == 0 {
		return evidence, fail(InfrastructureUnavailable, "no loopback interface")
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback == 0 || iface.Name != "lo" {
			return evidence, fail(InfrastructureUnavailable, "non-loopback interface: "+iface.Name)
		}
		addresses, e := iface.Addrs()
		if e != nil {
			return evidence, e
		}
		for _, address := range addresses {
			ip, _, e := net.ParseCIDR(address.String())
			if e != nil || !ip.IsLoopback() {
				return evidence, fail(InfrastructureUnavailable, "non-loopback address")
			}
		}
		evidence.Interfaces = append(evidence.Interfaces, iface.Name)
	}
	for _, route := range []struct {
		path   string
		target *string
	}{{"/proc/net/route", &evidence.IPv4Routes}, {"/proc/net/ipv6_route", &evidence.IPv6Routes}} {
		data, e := os.ReadFile(route.path)
		if e != nil || len(data) > 64<<10 {
			return evidence, fail(InfrastructureUnavailable, fmt.Sprintf("route evidence unavailable: %s", route.path))
		}
		*route.target = string(data)
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		for i, line := range lines {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			if route.path == "/proc/net/route" {
				if i == 0 && fields[0] == "Iface" {
					continue
				}
				if fields[0] != "lo" {
					return evidence, fail(InfrastructureUnavailable, "IPv4 route leaves loopback")
				}
			} else if fields[len(fields)-1] != "lo" {
				return evidence, fail(InfrastructureUnavailable, "IPv6 route leaves loopback")
			}
		}
	}
	evidence.LoopbackOnly = true
	return evidence, nil
}
