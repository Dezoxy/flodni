package config

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// "Failed to parse" is a line operators grep for and the startup report's
// config_parse_failed alarm.
func TestUnparsableConfigIsLoggedAndFlagged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"metrics_port": `), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MKV_PROXY_CONFIG_PATH", path)
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr); ParseFailed.Store(false) })

	_, _ = LoadConfig()

	if !strings.Contains(buf.String(), "[Config] WARNING: Failed to parse "+path+": ") {
		t.Fatalf("log = %q", buf.String())
	}
	if !ParseFailed.Load() {
		t.Fatal("ParseFailed not set")
	}

	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _ = LoadConfig()
	if ParseFailed.Load() {
		t.Fatal("ParseFailed must clear after a good load")
	}
}

// loadCfg is LoadConfig for the tests that read a default or a valid file and
// have no use for the error.
func loadCfg() Config {
	c, _ := LoadConfig()
	return c
}
