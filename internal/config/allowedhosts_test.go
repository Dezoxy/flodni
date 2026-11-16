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
	if got := loadCfg().AllowedHosts; len(got) != 0 {
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
	c.finalize()
	if got := c.EngineAllowedHosts(); got != nil {
		t.Fatalf("allowlist off: got %v, want nil", got)
	}
	c.AllowedHosts = []string{"x.example"}
	c.finalize()
	if got := c.EngineAllowedHosts(); !reflect.DeepEqual(got, []string{"x.example", "engine"}) {
		t.Fatalf("engine: got %v, want the list plus the engine host", got)
	}
	if got := c.EffectiveAllowedHosts(); !reflect.DeepEqual(got, []string{"x.example"}) {
		t.Fatalf("control: got %v, want the list only", got)
	}
}

// A list that is non-empty as written but has no valid entry must not turn the
// check off: it stays on with only the always-allowed hosts.
func TestAllInvalidAllowedHostsFailsClosed(t *testing.T) {
	t.Setenv("MKV_PROXY_ALLOWED_HOSTS", "")
	c := loadWith(t, `{"gostorm_url": "http://engine:8090",
		"allowed_hosts": ["https://x.example", "*.example"]}`)
	if len(c.AllowedHosts) != 0 {
		t.Fatalf("allowed_hosts = %v, want none valid", c.AllowedHosts)
	}
	if got := c.EngineAllowedHosts(); !reflect.DeepEqual(got, []string{"localhost", "engine"}) {
		t.Fatalf("engine = %v, want the always-allowed host plus the gostorm_url host (check stays on)", got)
	}
	if got := c.EffectiveAllowedHosts(); !reflect.DeepEqual(got, []string{"localhost"}) {
		t.Fatalf("control = %v, want only the always-allowed hosts (check stays on)", got)
	}
	c = loadWith(t, `{"gostorm_url": "", "allowed_hosts": ["/x"]}`)
	if got := c.EngineAllowedHosts(); len(got) == 0 {
		t.Fatal("check turned off with every entry invalid and no gostorm_url host")
	}
	t.Setenv("MKV_PROXY_ALLOWED_HOSTS", "https://a.example")
	envCfg := loadWith(t, `{}`)
	if got := envCfg.EffectiveAllowedHosts(); len(got) == 0 {
		t.Fatal("env list with only invalid entries turned the check off")
	}
	t.Setenv("MKV_PROXY_ALLOWED_HOSTS", "")
	blankCfg := loadWith(t, `{"allowed_hosts": ["", " "]}`)
	if got := blankCfg.EffectiveAllowedHosts(); got != nil {
		t.Fatalf("blank list = %v, want off", got)
	}
}

// A mistyped allowed_hosts (a string, not a list) and an env value with no entry
// both keep the check on instead of turning it off.
func TestMistypedAllowedHostsFailsClosed(t *testing.T) {
	t.Setenv("MKV_PROXY_ALLOWED_HOSTS", "")
	c := loadWith(t, `{"allowed_hosts": "flodni.example"}`)
	if got := c.EffectiveAllowedHosts(); len(got) == 0 {
		t.Fatal("a string allowed_hosts turned the check off")
	}
	t.Setenv("MKV_PROXY_ALLOWED_HOSTS", ",")
	c = loadWith(t, `{"allowed_hosts": ["x.example"]}`)
	if got := c.EffectiveAllowedHosts(); !reflect.DeepEqual(got, []string{"localhost"}) {
		t.Fatalf("env %q over a file list: %v, want only the always-allowed hosts", ",", got)
	}
}

func TestValidateAllowedHosts(t *testing.T) {
	for _, ok := range [][]string{nil, {}, {"x.example", "flodni:9080", "192.168.1.5", "[::1]"}, {"", " "}} {
		if err := ValidateAllowedHosts(ok); err != nil {
			t.Errorf("%v: %v, want valid", ok, err)
		}
	}
	for _, bad := range []string{"https://x.example", "x.example/p", "*.example", ".example.com", "a b"} {
		if err := ValidateAllowedHosts([]string{"ok.example", bad}); err == nil {
			t.Errorf("%q accepted, want an error", bad)
		}
	}
}

func TestLeadingDotDroppedAndNonASCIIWarned(t *testing.T) {
	t.Setenv("MKV_PROXY_ALLOWED_HOSTS", "")
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	got := loadWith(t, `{"allowed_hosts": [".example.com", "fl\u00f6dni.example"]}`).AllowedHosts
	if !reflect.DeepEqual(got, []string{"fl\u00f6dni.example"}) {
		t.Fatalf("allowed_hosts = %q, want only the non-ASCII entry kept", got)
	}
	if !strings.Contains(buf.String(), ".example.com") || !strings.Contains(buf.String(), "xn--") {
		t.Fatalf("want a warning for each entry, got:\n%s", buf.String())
	}
}

// What a start would enforce if a posted config were saved, compared as a set.
func TestHostsSaveChanged(t *testing.T) {
	t.Setenv("MKV_PROXY_ALLOWED_HOSTS", "")
	disk := loadWith(t, `{"gostorm_url": "http://127.0.0.1:8090",
		"allowed_hosts": ["x.example"]}`)
	run := disk
	post := func(url string, hosts []string) bool {
		p := Config{GoStormBaseURL: url, AllowedHosts: hosts}
		return disk.HostsSaveChanged(&p, &run)
	}
	for name, tc := range map[string]struct {
		url   string
		hosts []string
		want  bool
	}{
		"same":                  {"http://127.0.0.1:8090", []string{"x.example"}, false},
		"absent list":           {"http://127.0.0.1:8090", nil, false},
		"port only":             {"http://127.0.0.1:9999", []string{"x.example"}, false},
		"ip literal":            {"http://10.0.0.5:8090", []string{"x.example"}, false},
		"localhost":             {"http://localhost:8090", []string{"x.example"}, false},
		"new name":              {"http://attacker.example:8090", []string{"x.example"}, true},
		"new name, absent list": {"http://attacker.example:8090", nil, true},
		"widened list":          {"http://127.0.0.1:8090", []string{"x.example", "y.example"}, true},
		"cleared list":          {"http://127.0.0.1:8090", []string{}, true},
		"reordered":             {"http://127.0.0.1:8090", []string{"x.example"}, false},
	} {
		if got := post(tc.url, tc.hosts); got != tc.want {
			t.Errorf("%s: changed = %v, want %v", name, got, tc.want)
		}
	}
	// A post equal to the running list is unchanged even when the file was edited by hand.
	run = loadWith(t, `{"gostorm_url": "http://127.0.0.1:8090", "allowed_hosts": ["old.example"]}`)
	if post("http://127.0.0.1:8090", []string{"old.example"}) {
		t.Error("a post equal to the running list counted as a change")
	}
}
