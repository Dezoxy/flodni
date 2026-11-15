package settings

import (
	"bytes"
	"errors"
	"fmt"
	stdlog "log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	bolt "go.etcd.io/bbolt"
)

// withFreshDB points the package at dir with no open database, and restores the
// globals when the test ends.
func withFreshDB(t *testing.T, dir string) {
	t.Helper()
	oldPath, oldGlobal, oldTdb := Path, globalBboltDB, tdb
	Path, globalBboltDB = dir, nil
	t.Cleanup(func() {
		if globalBboltDB != nil {
			globalBboltDB.CloseDB()
		}
		Path, globalBboltDB, tdb = oldPath, oldGlobal, oldTdb
	})
}

func TestConfigDBCreatedOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	withFreshDB(t, dir)
	if NewTDB() == nil {
		t.Fatal("open failed")
	}
	fi, err := os.Stat(filepath.Join(dir, "config.db"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %v, want 0600", got)
	}
}

func TestConfigDBWiderModeIsTightened(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.db")
	db, err := bolt.Open(path, 0o666, nil)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	withFreshDB(t, dir)
	if NewTDB() == nil {
		t.Fatal("open failed")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %v, want 0600", got)
	}
}

// Closing the database releases the file lock: a reopen takes it at once instead of
// waiting out bbolt's timeout.
func TestCloseDBReleasesTheFile(t *testing.T) {
	dir := t.TempDir()
	withFreshDB(t, dir)
	db := NewTDB()
	if db == nil {
		t.Fatal("open failed")
	}
	tdb = db
	CloseDB()

	start := time.Now()
	again, err := bolt.Open(filepath.Join(dir, "config.db"), 0o600, &bolt.Options{Timeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("reopen after CloseDB: %v", err)
	}
	again.Close()
	if took := time.Since(start); took > time.Second {
		t.Errorf("reopen took %v, want immediate", took)
	}
	// A late write fails inside bbolt; it must not panic.
	db.Set("Torrents", "late", []byte(`{}`))
	CloseDB() // twice is harmless
}

const crashChildEnv = "FLODNI_TEST_BOLT_CRASH_DIR"

// The child commits records one by one, printing each key after Set returns, until
// the parent kills it.
func TestMain(m *testing.M) {
	if dir := os.Getenv(crashChildEnv); dir != "" {
		Path = dir
		db := NewTDB()
		if db == nil {
			os.Exit(2)
		}
		val := make([]byte, 4096)
		for i := 0; ; i++ {
			db.Set("Torrents", fmt.Sprintf("rec%06d", i), val)
			fmt.Printf("%d\n", i)
		}
	}
	os.Exit(m.Run())
}

// SIGKILL mid-write: the file reopens and every record the child reported as committed
// is there. A process kill leaves the page cache intact, so this guards against a torn
// file, not against a missing fsync: TestConfigDBIsSynced pins that.
func TestConfigDBSurvivesKill(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), crashChildEnv+"="+dir)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	last := -1
	for last < 20 {
		var n int
		if _, err := fmt.Fscan(out, &n); err != nil {
			t.Fatalf("child output ended at %d: %v", last, err)
		}
		last = n
	}
	// Keep reading while killing, so the child is mid-write when it dies.
	done := make(chan int, 1)
	go func() {
		l := last
		for {
			var n int
			if _, err := fmt.Fscan(out, &n); err != nil {
				done <- l
				return
			}
			l = n
		}
	}()
	time.Sleep(50 * time.Millisecond)
	_ = cmd.Process.Signal(syscall.SIGKILL)
	_ = cmd.Wait()
	last = <-done

	db, err := bolt.Open(filepath.Join(dir, "config.db"), 0o600, &bolt.Options{Timeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("reopen after kill: %v", err)
	}
	defer db.Close()
	if err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("Torrents"))
		if b == nil {
			return fmt.Errorf("bucket missing, child committed %d", last)
		}
		for i := 0; i <= last; i++ {
			if b.Get([]byte(fmt.Sprintf("rec%06d", i))) == nil {
				return fmt.Errorf("record %d of %d committed is gone", i, last)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Logf("child committed %d records before the kill; all readable", last+1)
}

// The headline change: every commit is synced. A kill test cannot tell, so assert the option.
func TestConfigDBIsSynced(t *testing.T) {
	withFreshDB(t, t.TempDir())
	db, ok := NewTDB().(*TDB)
	if !ok {
		t.Fatal("NewTDB did not return a *TDB")
	}
	if db.db.NoSync {
		t.Error("config.db is open with NoSync: a commit is not fsynced")
	}
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	stdlog.SetOutput(&buf)
	t.Cleanup(func() { stdlog.SetOutput(os.Stderr) })
	return &buf
}

// A failed write names the key and the size, never the record: it holds tracker URLs
// with passkeys.
func TestWriteErrorDoesNotLogTheRecord(t *testing.T) {
	withFreshDB(t, t.TempDir())
	db := NewTDB()
	db.CloseDB() // every write now fails inside bbolt
	buf := captureLog(t)

	secret := `{"trackers":["http://tracker.example/announce?passkey=SECRETPASSKEY"]}`
	db.Set("Torrents", "abc", []byte(secret))

	out := buf.String()
	if !strings.Contains(out, "Torrents/abc") {
		t.Errorf("log should name the key:\n%s", out)
	}
	for _, bad := range []string{"announce", "://", "SECRETPASSKEY", "123 34"} {
		if strings.Contains(out, bad) {
			t.Errorf("log leaks the record (%q):\n%s", bad, out)
		}
	}
}

// Through the wrapper production uses: after close a write is refused and is not
// readable from the cache, and a read does not return data.
func TestReadCacheAfterClose(t *testing.T) {
	withFreshDB(t, t.TempDir())
	raw := NewTDB()
	cache := NewDBReadCache(raw)
	tdb = cache
	cache.Set("Torrents", "before", []byte(`{"a":1}`))
	if string(cache.Get("Torrents", "before")) != `{"a":1}` {
		t.Fatal("record not readable before close")
	}
	CloseDB()
	buf := captureLog(t)

	cache.Set("Torrents", "after", []byte(`{"b":2}`)) // must not panic
	if got := cache.Get("Torrents", "after"); got != nil {
		t.Errorf("a write refused after close reads back as %q", got)
	}
	if got := cache.Get("Torrents", "before"); got != nil {
		t.Errorf("a read after close returns cached data %q", got)
	}
	if got := cache.List("Torrents"); got != nil {
		t.Errorf("List after close = %v, want nil", got)
	}
	if !strings.Contains(buf.String(), "closed") {
		t.Errorf("a refused write should be logged:\n%s", buf.String())
	}
	CloseDB() // twice is harmless
}

// Wake saves the record it has just read. An identical record must not commit again.
func TestUnchangedRecordIsNotRecommitted(t *testing.T) {
	withFreshDB(t, t.TempDir())
	raw := NewTDB().(*TDB)
	tdb = NewDBReadCache(raw)

	rec := &TorrentDB{
		TorrentSpec: &torrent.TorrentSpec{InfoBytes: []byte("info"), PeerAddrs: []string{"192.0.2.1:1"}},
		Title:       "t",
	}
	rec.InfoHash = metainfo.Hash{1}
	writes := func() int64 { st := raw.db.Stats(); return st.TxStats.GetWrite() }

	AddTorrent(rec)
	first := writes()
	if first == 0 {
		t.Fatal("first save wrote nothing")
	}
	for i := 0; i < 3; i++ {
		AddTorrent(rec)
	}
	if got := writes(); got != first {
		t.Errorf("identical saves wrote %d more pages, want 0", got-first)
	}
	rec.Title = "changed"
	AddTorrent(rec)
	if writes() == first {
		t.Error("a changed record was not written")
	}
}

// flakyDB is a TDB whose commits fail while fail is set, as a full disk or an I/O error
// would: Set swallows the failure, trySet reports it.
type flakyDB struct {
	*TDB
	fail bool
}

func (f *flakyDB) Set(xPath, name string, value []byte) { _ = f.trySet(xPath, name, value) }

func (f *flakyDB) trySet(xPath, name string, value []byte) error {
	if f.fail {
		return errors.New("injected: no space left on device")
	}
	return f.TDB.trySet(xPath, name, value)
}

func (f *flakyDB) tryRem(xPath, name string) error {
	if f.fail {
		return errors.New("injected: no space left on device")
	}
	return f.TDB.tryRem(xPath, name)
}

func (f *flakyDB) tryClear(xPath string) error {
	if f.fail {
		return errors.New("injected: no space left on device")
	}
	return f.TDB.tryClear(xPath)
}

// The cache holds committed data only. A failed commit must not leave the record in the
// cache, or the identical save after the disk recovers would be skipped as unchanged and
// the record would never reach the file.
func TestFailedCommitIsRetriedByTheNextSave(t *testing.T) {
	withFreshDB(t, t.TempDir())
	flaky := &flakyDB{TDB: NewTDB().(*TDB)}
	tdb = NewDBReadCache(flaky)

	rec := &TorrentDB{
		TorrentSpec: &torrent.TorrentSpec{InfoBytes: []byte("info")},
		Title:       "t",
	}
	rec.InfoHash = metainfo.Hash{7}
	key := rec.InfoHash.HexString()
	onDisk := func() []byte {
		var got []byte
		_ = flaky.db.View(func(tx *bolt.Tx) error {
			if b := tx.Bucket([]byte("Torrents")); b != nil {
				got = append(got, b.Get([]byte(key))...)
			}
			return nil
		})
		return got
	}

	flaky.fail = true
	AddTorrent(rec)
	if len(onDisk()) != 0 {
		t.Fatal("record on disk after a failed commit")
	}
	if got := tdb.Get("Torrents", key); got != nil {
		t.Errorf("cache holds a record that was never committed: %q", got)
	}

	flaky.fail = false
	AddTorrent(rec)
	if len(onDisk()) == 0 {
		t.Error("the identical save after recovery did not commit the record")
	}
}
