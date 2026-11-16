package config

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"
)

func TestLogFormatDefaultsToTextAndFallsBackOnAnUnknownValue(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	for body, want := range map[string]string{
		`{}`:                     "text",
		`{"log_format": "json"}`: "json",
		`{"log_format": "JSON"}`: "json",
		`{"log_format": "text"}`: "text",
		`{"log_format": "xml"}`:  "text",
	} {
		buf.Reset()
		if got := loadWith(t, body).LogFormat; got != want {
			t.Errorf("%s: log_format = %q, want %q", body, got, want)
		}
		if warned := strings.Contains(buf.String(), "log_format"); warned != strings.Contains(body, "xml") {
			t.Errorf("%s: warned = %v: %s", body, warned, buf.String())
		}
	}

	t.Setenv("MKV_PROXY_LOG_FORMAT", "json")
	if got := loadWith(t, `{}`).LogFormat; got != "json" {
		t.Errorf("MKV_PROXY_LOG_FORMAT: log_format = %q, want json", got)
	}
}

func TestFailedToParseLineKeepsItsText(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	_, _ = loadWithErr(t, `{not json`)
	if !strings.Contains(buf.String(), "[Config] WARNING: Failed to parse ") {
		t.Fatalf("default output lost the stable line: %q", buf.String())
	}
}
