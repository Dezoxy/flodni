package daemon

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// A save over a config.json the daemon cannot read or parse answers 409 in every
// mode, not only on an open port (ADR 0004), and writes nothing.
func TestSaveOverABrokenFileIs409InEveryMode(t *testing.T) {
	for _, tc := range []struct {
		name, startToken, broken string
	}{
		{"open, unparsable", "", `{"log_level":"info",`},
		{"token, unparsable", "tok", `{"log_level":"info",`},
		{"token, type error", "tok", `{"log_level":"info","metrics_port":"9080"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, path, _ := controlAPIServer(t, tc.startToken, "")
			if err := os.WriteFile(path, []byte(tc.broken), 0o600); err != nil {
				t.Fatal(err)
			}
			code, resp := doControl(h, "/api/config", `{"log_level":"debug"}`, tc.startToken)
			if code != http.StatusConflict {
				t.Fatalf("status %d (%q), want 409", code, resp)
			}
			if after, _ := os.ReadFile(path); string(after) != tc.broken {
				t.Fatalf("a broken file was overwritten: %s", after)
			}
		})
	}
}

// `flodni healthcheck` reads the config for the port only. A file it cannot use
// is no reason to call the daemon unhealthy: it falls back to the default port.
func TestHealthcheckFallsBackToTheDefaultPort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("MKV_PROXY_CONFIG_PATH", path)
	t.Setenv("FLODNI_API_TOKEN", "")
	const def = "http://127.0.0.1:9080/api/health"

	if got := healthcheckURL(); got != def {
		t.Errorf("missing file: %s, want %s", got, def)
	}
	for name, body := range map[string]string{
		"unparsable": `{"metrics_port": 9191,`,
		"type error": `{"metrics_port": 9191, "log_level": 5}`,
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := healthcheckURL(); got != def {
			t.Errorf("%s: %s, want the default %s", name, got, def)
		}
	}
	if err := os.WriteFile(path, []byte(`{"metrics_port": 9191}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := healthcheckURL(); got != "http://127.0.0.1:9191/api/health" {
		t.Errorf("a good file keeps its port: %s", got)
	}
}
