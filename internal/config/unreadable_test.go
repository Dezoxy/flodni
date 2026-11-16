package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func pointAt(t *testing.T, path string) {
	t.Helper()
	t.Setenv("MKV_PROXY_CONFIG_PATH", path)
	t.Cleanup(func() { ParseFailed.Store(false) })
}

func writeFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	pointAt(t, path)
	return path
}

// A config.json that exists but is unusable must stop the start (ADR 0004): the
// error names the file and the cause, never what the file holds.
func TestLoadConfigRefusesAFileItCannotUse(t *testing.T) {
	t.Run("unparsable", func(t *testing.T) {
		path := writeFile(t, `{"api_token":"SECRETVALUE", "metrics_port": `)
		_, err := LoadConfig()
		if err == nil {
			t.Fatal("an unparsable config.json loaded without an error")
		}
		if !strings.Contains(err.Error(), path) || strings.Contains(err.Error(), "SECRETVALUE") {
			t.Fatalf("error must name the path and not leak content: %v", err)
		}
	})
	t.Run("type error", func(t *testing.T) {
		writeFile(t, `{"metrics_port": "9080", "log_level": "debug"}`)
		cfg, err := LoadConfig()
		if err == nil {
			t.Fatal("a type error loaded without an error")
		}
		if cfg.LogLevel != "debug" {
			t.Errorf("the other keys must still fill the returned config, log_level = %q", cfg.LogLevel)
		}
	})
	t.Run("unreadable", func(t *testing.T) {
		// A directory where the file should be: it exists and cannot be read,
		// whatever user the test runs as.
		dir := t.TempDir()
		pointAt(t, dir)
		_, err := LoadConfig()
		if err == nil || !strings.Contains(err.Error(), dir) {
			t.Fatalf("an unreadable config.json: err = %v, want one naming the path", err)
		}
	})
}

// A missing file stays valid, and so does one with // comments.
func TestLoadConfigAcceptsMissingAndCommentedFiles(t *testing.T) {
	pointAt(t, filepath.Join(t.TempDir(), "absent.json"))
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("a missing config.json is valid: %v", err)
	}
	if cfg.MetricsPort != 9080 || cfg.GoStormBaseURL != "http://127.0.0.1:8090" {
		t.Errorf("a missing file keeps the defaults: port %d, gostorm %q", cfg.MetricsPort, cfg.GoStormBaseURL)
	}

	writeFile(t, "{\n  // the port\n  \"metrics_port\": 9191 // trailing\n}\n")
	cfg, err = LoadConfig()
	if err != nil || cfg.MetricsPort != 9191 {
		t.Fatalf("a commented file: port %d, err %v", cfg.MetricsPort, err)
	}
}
