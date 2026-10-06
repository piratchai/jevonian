package darwin_test

import (
	"strings"
	"testing"

	"github.com/xinyao27/jevonian/internal/platform/darwin"
)

const enabled = `<dictionary> {
  ExceptionsList : <array> {
    0 : 119.29.29.29.dns
    1 : *.abchina.com.cn
    2 : 192.168.0.0/16
    3 : *.local
    4 : localhost
  }
  ExcludeSimpleHostnames : 1
  FTPPassive : 1
  HTTPEnable : 1
  HTTPPort : 1082
  HTTPProxy : 127.0.0.1
  HTTPSEnable : 1
  HTTPSPort : 1082
  HTTPSProxy : 127.0.0.1
  ProxyAutoConfigEnable : 0
  SOCKSEnable : 0
  SOCKSProxy : 127.0.0.1
}`

const httpOnly = `<dictionary> {
  HTTPEnable : 1
  HTTPPort : 7890
  HTTPProxy : 10.0.0.2
  HTTPSEnable : 0
  ProxyAutoConfigEnable : 1
  ProxyAutoConfigURLString : http://wpad/wpad.dat
}`

const disabled = `<dictionary> {
  ExcludeSimpleHostnames : 0
  HTTPEnable : 0
  HTTPSEnable : 0
  ProxyAutoConfigEnable : 0
  SOCKSEnable : 0
}`

const empty = "<dictionary> {\n}"

func TestParseScutilProxyEnabled(t *testing.T) {
	got := darwin.ParseScutilProxy(enabled)
	if got == nil {
		t.Fatal("expected proxy, got nil")
	}
	if got.URL != "http://127.0.0.1:1082" {
		t.Fatalf("URL = %q, want http://127.0.0.1:1082", got.URL)
	}
	wantBypass := []string{
		"localhost",
		"127.0.0.1",
		"::1",
		"119.29.29.29.dns",
		"*.abchina.com.cn",
		"192.168.0.0/16",
		"*.local",
	}
	if len(got.Bypass) != len(wantBypass) {
		t.Fatalf("Bypass = %#v, want %#v", got.Bypass, wantBypass)
	}
	for i, host := range wantBypass {
		if got.Bypass[i] != host {
			t.Fatalf("Bypass[%d] = %q, want %q", i, got.Bypass[i], host)
		}
	}
}

func TestParseScutilProxyHTTPOnly(t *testing.T) {
	got := darwin.ParseScutilProxy(httpOnly)
	if got == nil {
		t.Fatal("expected proxy, got nil")
	}
	if got.URL != "http://10.0.0.2:7890" {
		t.Fatalf("URL = %q, want http://10.0.0.2:7890", got.URL)
	}
	if strings.Contains(got.URL, "wpad") {
		t.Fatalf("must not mistake a PAC URL for a proxy: %q", got.URL)
	}
}

func TestParseScutilProxyDisabled(t *testing.T) {
	if darwin.ParseScutilProxy(disabled) != nil {
		t.Fatal("expected nil for disabled proxy")
	}
	if darwin.ParseScutilProxy(empty) != nil {
		t.Fatal("expected nil for empty dictionary")
	}
}
