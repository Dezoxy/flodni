package daemon

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"flodni/internal/config"
	"flodni/internal/flog"
)

// A save over a config.json the daemon cannot read or parse answers 409 in every
// mode, not only on an open port (ADR 0004), and writes nothing.
func TestSaveOverABrokenFileIs409InEveryMode(t *testing.T) {
	for _, tc := range []struct {
		name, startToken, broken string
	}{
		{"open, unparsable", "", `{"log_level":"info",`},
		{"token, unparsable", "tok", `{"log_level":"info",`},
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

// An unreadable file (here a directory where the file should be) is refused the
// same way, and the log says why: the path and the cause, never content.
func TestSaveOverAnUnreadableFileIs409AndLogsWhy(t *testing.T) {
	for _, tok := range []string{"", "tok"} {
		h, path, _ := controlAPIServer(t, tok, "")
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		logger = flog.NewText(&buf)
		code, resp := doControl(h, "/api/config", `{"log_level":"debug","api_token":""}`, tok)
		if code != http.StatusConflict {
			t.Fatalf("token %q: status %d (%s), want 409", tok, code, resp)
		}
		if !strings.Contains(buf.String(), path) || !strings.Contains(buf.String(), "cannot be read") {
			t.Fatalf("token %q: the log must name the file and the cause: %q", tok, buf.String())
		}
	}
}

// What a save is about to write must load, or nothing is written: 400.
func TestSaveThatWouldNotLoadIs400AndWritesNothing(t *testing.T) {
	h, path, _ := controlAPIServer(t, "tok", "tok")
	before, _ := os.ReadFile(path)
	prev := checkLoadable
	checkLoadable = func([]byte) error { return errors.New("forced") }
	t.Cleanup(func() { checkLoadable = prev })
	code, resp := doControl(h, "/api/config", `{"log_level":"debug"}`, "tok")
	if code != http.StatusBadRequest {
		t.Fatalf("status %d (%s), want 400", code, resp)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatalf("the file was written: %s", after)
	}
}

// Posts whose strings hold // used to be written as a file the loader cut apart.
func TestSaveWithSlashesInValuesStillLoads(t *testing.T) {
	h, path, _ := controlAPIServer(t, "tok", "tok")
	body := `{"log_level":"a//b","physical_source_path":"/mnt//media","gostorm_url":"http://127.0.0.1:8090//"}`
	if code, resp := doControl(h, "/api/config", body, "tok"); code != http.StatusOK {
		t.Fatalf("status %d (%s), want 200", code, resp)
	}
	if _, err := config.LoadConfig(); err != nil {
		t.Fatalf("the saved file does not load (%s): %v", path, err)
	}
	if got := gc().LogLevel; got != "a//b" {
		t.Fatalf("live log_level = %q", got)
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
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := healthcheckURL(); got != def {
			t.Errorf("%s: %s, want the default %s", name, got, def)
		}
	}
	// A type error in another key leaves the port that decoded.
	if err := os.WriteFile(path, []byte(`{"metrics_port": 9191, "log_level": 5}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := healthcheckURL(); got != "http://127.0.0.1:9191/api/health" {
		t.Errorf("a type error elsewhere must keep the decoded port: %s", got)
	}
	if err := os.WriteFile(path, []byte(`{"metrics_port": 9191}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := healthcheckURL(); got != "http://127.0.0.1:9191/api/health" {
		t.Errorf("a good file keeps its port: %s", got)
	}
}
