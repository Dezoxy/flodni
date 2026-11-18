package tlsverify

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func reset(t *testing.T) *[]string {
	t.Helper()
	var lines []string
	prevWarn, prevNow := Warn, now
	Warn = func(s string) { lines = append(lines, s) }
	t.Cleanup(func() { Warn, now = prevWarn, prevNow; resetForTest() })
	resetForTest()
	return &lines
}

func tlsServer(t *testing.T) *httptest.Server {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(srv.Close)
	return srv
}

func get(c *http.Client, u string) error {
	resp, err := c.Get(u)
	if err == nil {
		resp.Body.Close()
	}
	return err
}

func TestClassifyRealCertificateErrors(t *testing.T) {
	srv := tlsServer(t)

	// the test CA is not trusted by a default client
	err := get(&http.Client{}, srv.URL+"/announce/SECRETKEY?passkey=SECRETKEY")
	label, text, ok := Classify(err)
	if !ok || label != "unknown_authority" || text != "unknown authority" {
		t.Fatalf("self-signed: %q %q %v (%v)", label, text, ok, err)
	}

	// trusted CA, wrong name: the server's certificate does not cover "localhost"
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	err = get(srv.Client(), "https://localhost:"+port+"/")
	label, text, ok = Classify(err)
	if !ok || label != "hostname" || text != "hostname mismatch" {
		t.Fatalf("hostname: %q %q %v (%v)", label, text, ok, err)
	}
}

func TestClassifyExpiredNotYetValidAndOther(t *testing.T) {
	prev := now
	t.Cleanup(func() { now = prev })
	now = func() time.Time { return time.Unix(1_000_000, 0) }
	wrap := func(e error) error {
		return &url.Error{Op: "Get", URL: "https://x/", Err: &tls.CertificateVerificationError{Err: e}}
	}

	past := &x509.Certificate{NotBefore: time.Unix(0, 0), NotAfter: time.Unix(10, 0)}
	future := &x509.Certificate{NotBefore: time.Unix(2_000_000, 0), NotAfter: time.Unix(3_000_000, 0)}
	for _, tc := range []struct {
		err         error
		label, text string
	}{
		{wrap(x509.CertificateInvalidError{Cert: past, Reason: x509.Expired}), "expired", "expired"},
		{wrap(x509.CertificateInvalidError{Cert: future, Reason: x509.Expired}), "expired", "not yet valid"},
		{wrap(x509.CertificateInvalidError{Cert: past, Reason: x509.NotAuthorizedToSign}), "other", "other"},
		{wrap(x509.SystemRootsError{}), "other", "other"},
	} {
		label, text, ok := Classify(tc.err)
		if !ok || label != tc.label || text != tc.text {
			t.Errorf("%v: got %q %q %v, want %q %q", tc.err, label, text, ok, tc.label, tc.text)
		}
	}
	if _, _, ok := Classify(&url.Error{Op: "Get", Err: net.UnknownNetworkError("x")}); ok {
		t.Error("a non-certificate error was classified")
	}
	if _, _, ok := Classify(nil); ok {
		t.Error("nil was classified")
	}
}

func TestReportWarnsOncePerHostPerHourAndCounts(t *testing.T) {
	lines := reset(t)
	clock := time.Unix(1_000_000, 0)
	now = func() time.Time { return clock }
	srv := tlsServer(t)
	err := get(&http.Client{}, srv.URL+"/announce/SECRETKEY?passkey=SECRETKEY")
	hp := strings.TrimPrefix(srv.URL, "https://")

	Report(hp, err)
	Report(hp, err)
	Report(hp, err)
	if len(*lines) != 1 {
		t.Fatalf("%d warnings within the hour, want 1: %q", len(*lines), *lines)
	}
	if want := "[Tracker] TLS verify failed for " + hp + ": unknown authority"; (*lines)[0] != want {
		t.Fatalf("warning %q, want %q", (*lines)[0], want)
	}
	if strings.Contains((*lines)[0], "SECRETKEY") || strings.Contains((*lines)[0], "http") || strings.Contains((*lines)[0], "/") {
		t.Fatalf("warning leaks the URL: %q", (*lines)[0])
	}
	if c := Counts()["unknown_authority"]; c != 3 {
		t.Fatalf("counter %d, want 3 (every failure counts, only the log is limited)", c)
	}

	// another host is its own limit; the first again after an hour
	Report("other.example:443", err)
	if len(*lines) != 2 {
		t.Fatalf("second host not logged: %q", *lines)
	}
	clock = clock.Add(time.Hour + time.Second)
	Report(hp, err)
	if len(*lines) != 3 {
		t.Fatalf("not logged again after an hour: %q", *lines)
	}

	// a failure that is not a certificate one counts and logs nothing
	Report(hp, net.UnknownNetworkError("x"))
	if len(*lines) != 3 {
		t.Fatalf("non-TLS error logged: %q", *lines)
	}
}

func TestCountsHasEveryReasonFromZero(t *testing.T) {
	reset(t)
	got := Counts()
	for _, r := range []string{"unknown_authority", "hostname", "expired", "other"} {
		if v, ok := got[r]; !ok || v != 0 {
			t.Errorf("reason %q = %d, present %v; want present at 0", r, v, ok)
		}
	}
	if len(got) != 4 {
		t.Errorf("reasons %v, want exactly four", got)
	}
}
