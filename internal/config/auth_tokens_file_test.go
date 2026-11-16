package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestAuthTokensFileKeyAndEnv(t *testing.T) {
	var c Config
	if err := json.Unmarshal([]byte(`{"auth_tokens_file":"/etc/flodni/tokens.json"}`), &c); err != nil {
		t.Fatal(err)
	}
	if c.AuthTokensFile != "/etc/flodni/tokens.json" {
		t.Fatalf("key not read: %q", c.AuthTokensFile)
	}
	t.Setenv("FLODNI_AUTH_TOKENS_FILE", "/run/secrets/tokens.json")
	c.applyEnvOverrides()
	if c.AuthTokensFile != "/run/secrets/tokens.json" {
		t.Fatalf("the variable must win: %q", c.AuthTokensFile)
	}
	t.Setenv("FLODNI_AUTH_TOKENS_FILE", "")
	c.AuthTokensFile = "/etc/flodni/tokens.json"
	c.applyEnvOverrides()
	if c.AuthTokensFile != "/etc/flodni/tokens.json" {
		t.Fatalf("an empty variable must leave the file's value: %q", c.AuthTokensFile)
	}
}

// The pointer to the tokens file lives in config.json, so a file that does not load
// must stop the start in every mode: a wrong type, an array, a trailing comma, a file
// that cannot be read. Open mode and legacy mode (FLODNI_API_TOKEN set) both.
func TestUnusableConfigRefusesWhereTheTokensFileIsNamed(t *testing.T) {
	bodies := map[string]string{
		"number":         `{"auth_tokens_file": 5}`,
		"array":          `{"auth_tokens_file": ["/etc/flodni/t.json"]}`,
		"trailing comma": `{"auth_tokens_file": "/etc/flodni/t.json",}`,
	}
	for _, apiToken := range []string{"", "legacy-token"} {
		for name, body := range bodies {
			t.Run(name+"/api_token="+apiToken, func(t *testing.T) {
				p := filepath.Join(t.TempDir(), "config.json")
				if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("MKV_PROXY_CONFIG_PATH", p)
				t.Setenv("FLODNI_API_TOKEN", apiToken)
				t.Setenv("FLODNI_AUTH_TOKENS_FILE", "")
				if _, err := LoadConfig(); err == nil {
					t.Fatal("loaded; the start must refuse")
				}
			})
		}
		t.Run("unreadable/api_token="+apiToken, func(t *testing.T) {
			if os.Geteuid() == 0 {
				t.Skip("root reads a mode-000 file")
			}
			p := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(p, []byte(`{"auth_tokens_file":"/etc/flodni/t.json"}`), 0o000); err != nil {
				t.Fatal(err)
			}
			t.Setenv("MKV_PROXY_CONFIG_PATH", p)
			t.Setenv("FLODNI_API_TOKEN", apiToken)
			if _, err := LoadConfig(); err == nil {
				t.Fatal("loaded; the start must refuse")
			}
		})
	}
}
