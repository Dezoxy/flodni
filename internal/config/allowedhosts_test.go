package config

import (
	"bytes"
	"log"
	"os"
	"reflect"
	"strings"
	"testing"
)

// allowed_hosts is the optional Host allowlist (see internal/apiauth). Empty is
// off. Entries are lowercased, stripped of port and trailing dot, de-duplicated;
// one with a scheme, a path or any other non-hostname is dropped with a warning.

func TestAllowedHostsDefaultsToOff(t *testing.T) {
	t.Setenv("MKV_PROXY_ALLOWED_HOSTS", "")
	if got := LoadConfig().AllowedHosts; len(got) != 0 {
		t.Fatalf("allowed_hosts = %v, want empty", got)
	}
}

func TestAllowedHostsNormalised(t *testing.T) {
	t.Setenv("MKV_PROXY_ALLOWED_HOSTS", "")
	got := loadWith(t, `{"allowed_hosts": ["X.Example", "flodni:9080", "nas.lan.", "[::1]:80", "x.example", "  ", "192.168.1.5"]}`).AllowedHosts
	want := []string{"x.example", "flodni", "nas.lan", "::1", "192.168.1.5"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("allowed_hosts = %v, want %v", got, want)
	}
}

func TestAllowedHostsRejectsSchemePathAndJunk(t *testing.T) {
	t.Setenv("MKV_PROXY_ALLOWED_HOSTS", "")
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	got := loadWith(t, `{"allowed_hosts": ["https://x.example", "x.example/panel", "u@x.example", "*.example", "a b", "ok.example"]}`).AllowedHosts
	if !reflect.DeepEqual(got, []string{"ok.example"}) {
		t.Fatalf("allowed_hosts = %v, want only ok.example", got)
	}
	if n := strings.Count(buf.String(), "allowed_hosts entry"); n != 5 {
		t.Fatalf("%d warnings, want 5 (one per dropped entry):\n%s", n, buf.String())
	}
}

func TestAllowedHostsEnvCommaListOverridesFile(t *testing.T) {
	t.Setenv("MKV_PROXY_ALLOWED_HOSTS", " A.Example , flodni:9080,,https://bad.example")
	got := loadWith(t, `{"allowed_hosts": ["file.example"]}`).AllowedHosts
	want := []string{"a.example", "flodni"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("allowed_hosts = %v, want %v", got, want)
	}
}

func TestEffectiveAllowedHostsAddsEngineHostOnlyWhenOn(t *testing.T) {
	c := Config{GoStormBaseURL: "http://engine:8090"}
	if got := c.EffectiveAllowedHosts(); got != nil {
		t.Fatalf("allowlist off: got %v, want nil", got)
	}
	c.AllowedHosts = []string{"x.example"}
	if got := c.EffectiveAllowedHosts(); !reflect.DeepEqual(got, []string{"x.example", "engine"}) {
		t.Fatalf("got %v, want the list plus the engine host", got)
	}
}
