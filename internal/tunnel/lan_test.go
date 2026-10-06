package tunnel

import (
	"net"
	"reflect"
	"testing"

	"github.com/xinyao27/jevonian/internal/config"
)

func ip(s string) net.IP { return net.ParseIP(s) }

func TestLanIPv4Addresses(t *testing.T) {
	list := func() ([]InterfaceAddr, error) {
		return []InterfaceAddr{
			{IP: ip("127.0.0.1"), Internal: true},
			{IP: ip("192.168.1.20")},
			{IP: ip("fe80::1")},
			{IP: ip("192.168.1.20")}, // dup on a second iface
			{IP: ip("10.0.0.4")},
		}, nil
	}
	got := LanIPv4Addresses(list)
	if !reflect.DeepEqual(got, []string{"192.168.1.20", "10.0.0.4"}) {
		t.Fatalf("%v", got)
	}

	loopbackOnly := func() ([]InterfaceAddr, error) {
		return []InterfaceAddr{{IP: ip("127.0.0.1"), Internal: true}}, nil
	}
	if got := LanIPv4Addresses(loopbackOnly); len(got) != 0 {
		t.Fatalf("%v", got)
	}
}

func TestIsReachableFromLan(t *testing.T) {
	for _, host := range []string{"0.0.0.0", "::", "*"} {
		if !IsReachableFromLan(host) {
			t.Fatalf("%s unreachable", host)
		}
	}
	for _, host := range []string{"192.168.1.5", "127.0.0.1"} {
		if IsReachableFromLan(host) {
			t.Fatalf("%s reachable", host)
		}
	}
}

func lanConfig(host string, port int, lan config.LanConfig) *config.Config {
	return &config.Config{
		Listen: config.ListenConfig{Host: host, Port: port},
		Lan:    lan,
	}
}

func TestLanPort(t *testing.T) {
	if LanPortOffset != 2 {
		t.Fatalf("offset %d", LanPortOffset)
	}
	if got := LanPort(lanConfig("127.0.0.1", 8787, config.LanConfig{Enabled: true})); got != 8789 {
		t.Fatalf("%d", got)
	}
	pin := 9500
	if got := LanPort(lanConfig("127.0.0.1", 8787, config.LanConfig{Enabled: true, Port: &pin})); got != 9500 {
		t.Fatalf("%d", got)
	}
}

func TestLanBindHost(t *testing.T) {
	if LanBindHost(config.LanConfig{Enabled: true}) != "0.0.0.0" {
		t.Fatal("default")
	}
	if LanBindHost(config.LanConfig{Enabled: true, Host: "10.0.0.9"}) != "10.0.0.9" {
		t.Fatal("named")
	}
}

func TestLanBaseURLs(t *testing.T) {
	cfg := lanConfig("127.0.0.1", 8787, config.LanConfig{Enabled: true})
	got := LanBaseURLs(cfg, []string{"192.168.1.20", "10.0.0.4"})
	want := []string{"http://192.168.1.20:8789/v1", "http://10.0.0.4:8789/v1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}

	named := lanConfig("127.0.0.1", 8787, config.LanConfig{Enabled: true, Host: "10.0.0.9"})
	if got := LanBaseURLs(named, []string{"192.168.1.20"}); !reflect.DeepEqual(got, []string{"http://10.0.0.9:8789/v1"}) {
		t.Fatalf("%v", got)
	}

	if got := LanBaseURLs(cfg, nil); len(got) != 0 {
		t.Fatalf("%v", got)
	}
}
