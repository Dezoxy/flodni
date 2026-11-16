package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// config.json decodes through encoding/json, which matches keys without regard to
// case and folds U+017F and U+212A, so a misspelt or re-cased auth key can set
// (or silently fail to set) an auth setting. Every top-level key that folds to
// "auth_" must be one of the exact known keys, or the start is refused: a typo
// must never leave the daemon open.
func TestLoadConfigRefusesAnAuthKeyItDoesNotKnow(t *testing.T) {
	for name, body := range map[string]string{
		"typo":               `{"auth_token_file": "/etc/flodni/tokens.json"}`,
		"another auth key":   `{"auth_mode": "scoped"}`,
		"upper case":         `{"AUTH_TOKENS_FILE": "/etc/flodni/tokens.json"}`,
		"mixed case":         `{"Auth_Tokens_File": "/etc/flodni/tokens.json"}`,
		"long s":             "{\"auth_tokenſ_file\": \"/p\"}",
		"escaped long s":     `{"auth_tokenſ_file": "/p"}`,
		"kelvin sign":        `{"auth_toKens_file": "/p"}`,
		"long s in prefix":   `{"auth_jwt": null, "AUTH_jwt": {}}`,
		"auth jwt upper":     `{"Auth_JWT": {"issuer": "x"}}`,
		"auth panel typo":    `{"auth_panel_http": true}`,
		"bare prefix":        `{"auth_": 1}`,
		"escaped underscore": `{"auth_tokens_files": "/p"}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := writeFile(t, body)
			_, err := LoadConfig()
			if err == nil {
				t.Fatal("an unknown auth key loaded without an error")
			}
			if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "auth") {
				t.Errorf("the refusal must name the file and say it is an auth key: %v", err)
			}
			if strings.Contains(err.Error(), "/p") && !strings.Contains(err.Error(), path) {
				t.Errorf("the refusal must not carry a value: %v", err)
			}
		})
	}
}

func TestLoadConfigRefusesADuplicateAuthKey(t *testing.T) {
	for name, body := range map[string]string{
		"tokens file twice":    `{"auth_tokens_file": "/a", "auth_tokens_file": ""}`,
		"tokens file twice 2":  `{"auth_tokens_file": "", "auth_tokens_file": "/a"}`,
		"jwt twice":            `{"auth_jwt": null, "auth_jwt": null}`,
		"panel https twice":    `{"auth_panel_https": true, "auth_panel_https": false}`,
		"escaped duplicate":    `{"auth_tokens_file": "/a", "auth_tokens_file": ""}`,
		"duplicate after body": `{"auth_tokens_file": "/a", "log_level": "debug", "auth_tokens_file": "/b"}`,
	} {
		t.Run(name, func(t *testing.T) {
			writeFile(t, body)
			if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "twice") {
				t.Fatalf("err = %v, want a duplicate-key refusal", err)
			}
		})
	}
}

// null leaves the string empty, which would read as "no tokens file": open mode.
func TestLoadConfigRefusesANullTokensFile(t *testing.T) {
	writeFile(t, `{"auth_tokens_file": null}`)
	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "null") {
		t.Fatalf("err = %v, want a null refusal", err)
	}
}

func TestLoadConfigAcceptsTheKnownAuthKeys(t *testing.T) {
	writeFile(t, `{
		// comments are stripped before the keys are read
		"api_token": "",
		"auth_tokens_file": "/etc/flodni/tokens.json",
		"auth_jwt": {"issuer": "https://i.example", "audience": "a", "jwks_url": "https://i.example/k", "header": "X-Jwt"},
		"auth_panel_https": true
	}`)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AuthTokensFile != "/etc/flodni/tokens.json" || cfg.AuthJWT == nil || !cfg.AuthPanelHTTPS {
		t.Errorf("auth settings not read: %+v %+v %v", cfg.AuthTokensFile, cfg.AuthJWT, cfg.AuthPanelHTTPS)
	}
	if len(cfg.UnknownKeys()) != 0 {
		t.Errorf("a known key listed as unknown: %v", cfg.UnknownKeys())
	}
}

// Any other unknown key only warns: an old config with a removed key still starts.
func TestLoadConfigListsUnknownKeysByName(t *testing.T) {
	writeFile(t, `{"removed_in_an_old_release": "SECRETVALUE", "torrserver_url": "http://x", "log_level": "debug", "another_one": {"a": 1}}`)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("an unknown non-auth key refused: %v", err)
	}
	got := strings.Join(cfg.UnknownKeys(), ",")
	if got != "another_one,removed_in_an_old_release" {
		t.Errorf("unknown keys = %q, want the two by name, sorted (torrserver_url is the legacy gostorm_url)", got)
	}
	if strings.Contains(got, "SECRETVALUE") {
		t.Error("a value reached the list")
	}
	if cfg.LogLevel != "debug" {
		t.Error("the known keys must still load")
	}
}

func TestConfigHasNoSaveThatWouldPersistTheEnvironment(t *testing.T) {
	if _, ok := any(&Config{}).(interface{ Save() error }); ok {
		t.Fatal("Config.Save is back: it would write the environment's overrides to config.json")
	}
}

// What the panel's save writes (json.Marshal of the Config) must not trip the
// unknown-key warning on the next start.
func TestAConfigAsSavedHasNoUnknownKeys(t *testing.T) {
	writeFile(t, `{}`)
	base, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(base, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, string(data))
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.UnknownKeys()) != 0 {
		t.Errorf("a saved config lists unknown keys: %v", cfg.UnknownKeys())
	}
}

// Any unknown key that contains "auth" once folded is refused, not only one that starts
// with auth_: a mistyped auth key must never leave the daemon open with a warning.
func TestLoadConfigRefusesAnyUnknownKeyContainingAuth(t *testing.T) {
	for _, key := range []string{"auth-tokens-file", "authTokensFile", "AuthTokensFile", "tokens_auth_file", "x_AUTH_y", "oauth_file"} {
		writeFile(t, `{"`+key+`": "/p"}`)
		if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "auth") {
			t.Errorf("%s: err = %v, want a refusal", key, err)
		}
	}
	writeFile(t, `{"author_note": "x"}`) // a word that merely contains "auth" is still refused: the rule is blunt on purpose
	if _, err := LoadConfig(); err == nil {
		t.Error("author_note loaded")
	}
}

// api_token gets the duplicate, fold and null refusals: the last duplicate wins in
// encoding/json, so {"api_token":"x","Api_Token":""} would start with no token.
func TestLoadConfigProtectsAPIToken(t *testing.T) {
	for name, body := range map[string]string{
		"duplicate":      `{"api_token":"x","api_token":""}`,
		"case variant":   `{"api_token":"x","Api_Token":""}`,
		"upper":          `{"API_TOKEN":"x"}`,
		"null":           `{"api_token":null}`,
		"escaped repeat": `{"api_token":"x","api_token":""}`,
		"kelvin variant": `{"api_toKen":"x"}`,
	} {
		t.Run(name, func(t *testing.T) {
			writeFile(t, body)
			if _, err := LoadConfig(); err == nil {
				t.Fatal("loaded")
			}
		})
	}
	writeFile(t, `{"api_token":""}`)
	if _, err := LoadConfig(); err != nil {
		t.Errorf("an empty api_token is fine: %v", err)
	}
	writeFile(t, `{"api_token":"x"}`)
	if cfg, err := LoadConfig(); err != nil || cfg.APIToken != "x" {
		t.Errorf("api_token: %v %q", err, cfg.APIToken)
	}
}

// auth_jwt is decoded strictly: a repeated or re-cased key inside it refuses (a case
// variant of allowed_emails would otherwise empty the allow-list silently).
func TestLoadConfigDecodesAuthJWTStrictly(t *testing.T) {
	base := `"issuer":"https://i.example","audience":"a","jwks_url":"https://i.example/k","header":"H"`
	for name, blk := range map[string]string{
		"duplicate key":     `{` + base + `,"audience":"b"}`,
		"case variant":      `{` + base + `,"Audience":"b"}`,
		"emails then empty": `{` + base + `,"allowed_emails":["a@x.example"],"Allowed_Emails":[]}`,
		"unknown key":       `{` + base + `,"extra":1}`,
		"escaped repeat":    `{` + base + `,"audience":"b"}`,
	} {
		t.Run(name, func(t *testing.T) {
			writeFile(t, `{"auth_jwt":`+blk+`}`)
			if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "auth_jwt") {
				t.Fatalf("err = %v, want an auth_jwt refusal", err)
			}
		})
	}
	writeFile(t, `{"auth_jwt":{`+base+`,"allowed_emails":["a@x.example"]}}`)
	cfg, err := LoadConfig()
	if err != nil || cfg.AuthJWT == nil || len(cfg.AuthJWT.AllowedEmails) != 1 {
		t.Errorf("a strict block: %v %+v", err, cfg.AuthJWT)
	}
}
