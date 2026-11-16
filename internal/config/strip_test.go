package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func loadBody(t *testing.T, body string) (Config, error) {
	t.Helper()
	writeFile(t, body)
	return LoadConfig()
}

// A // inside a JSON string is data, never a comment (roadmap Durable records,
// step 20): the four posts the review found, and the one-line URL lists.
func TestStripKeepsSlashesInsideStrings(t *testing.T) {
	cfg, err := loadBody(t, `{"log_level":"a//b","physical_source_path":"/mnt//media","api_token":"q3//Zk1x","gostorm_url":"http://127.0.0.1:8090//"}`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.LogLevel != "a//b" || cfg.PhysicalSourcePath != "/mnt//media" || cfg.APIToken != "q3//Zk1x" || cfg.GoStormBaseURL != "http://127.0.0.1:8090//" {
		t.Fatalf("a value was cut: %q %q %q %q", cfg.LogLevel, cfg.PhysicalSourcePath, cfg.APIToken, cfg.GoStormBaseURL)
	}
}

func TestStripOneLineListOfTwoURLs(t *testing.T) {
	cfg, err := loadBody(t, `{"plex":{"url":"https://p.example/a"},"gostorm_url":"https://g.example/b"}`)
	if err != nil {
		t.Fatalf("a line with two URLs must load: %v", err)
	}
	if cfg.Plex.URL != "https://p.example/a" || cfg.GoStormBaseURL != "https://g.example/b" {
		t.Fatalf("plex %q, gostorm %q", cfg.Plex.URL, cfg.GoStormBaseURL)
	}
}

func TestStripStillRemovesRealComments(t *testing.T) {
	cfg, err := loadBody(t, "// a header\n{\n  // full line\n  \"log_level\": \"debug\", // trailing\n  \"gostorm_url\": \"http://h:1\" // after a URL\n}\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.LogLevel != "debug" || cfg.GoStormBaseURL != "http://h:1" {
		t.Fatalf("log_level %q, gostorm_url %q", cfg.LogLevel, cfg.GoStormBaseURL)
	}
}

func TestStripHandlesEscapedQuotesInStrings(t *testing.T) {
	// The string holds an escaped quote before the //; the // must stay.
	cfg, err := loadBody(t, `{"log_level":"say \"hi\" // not a comment"} // real`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.LogLevel != `say "hi" // not a comment` {
		t.Fatalf("log_level = %q", cfg.LogLevel)
	}
	// A string ending in an escaped backslash closes at the next quote.
	cfg, err = loadBody(t, `{"log_level":"a\\"} // c`)
	if err != nil || cfg.LogLevel != `a\` {
		t.Fatalf("log_level %q, err %v", cfg.LogLevel, err)
	}
}

// Block comments were never supported: outside a string they still do not parse,
// inside one they are data.
func TestStripBlockCommentsAreOnlyData(t *testing.T) {
	cfg, err := loadBody(t, `{"log_level":"a/*b*/c"}`)
	if err != nil || cfg.LogLevel != "a/*b*/c" {
		t.Fatalf("log_level %q, err %v", cfg.LogLevel, err)
	}
}

// What the save handler writes must load: marshal, strip, unmarshal round trip.
func TestStripRoundTripsWhatTheSaveWrites(t *testing.T) {
	in := Config{LogLevel: "a//b", PhysicalSourcePath: "/mnt//media", APIToken: "q3//Zk1x", GoStormBaseURL: "http://127.0.0.1:8090//"}
	b, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	var out Config
	if err := json.Unmarshal(stripJSONComments(b), &out); err != nil {
		t.Fatalf("the saved file does not load: %v", err)
	}
	if out.LogLevel != in.LogLevel || out.PhysicalSourcePath != in.PhysicalSourcePath || out.APIToken != in.APIToken || out.GoStormBaseURL != in.GoStormBaseURL {
		t.Fatalf("round trip changed a value: %+v", out)
	}
}

// A symlink that points nowhere is a config file that exists and cannot be read.
func TestDanglingSymlinkConfigRefuses(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "config.json")
	if err := os.Symlink(filepath.Join(dir, "gone.json"), link); err != nil {
		t.Skip("no symlinks here:", err)
	}
	pointAt(t, link)
	if _, err := LoadConfig(); err == nil {
		t.Fatal("a dangling config.json symlink loaded the defaults")
	}
}
