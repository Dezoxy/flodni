package settings

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

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
// is there. This only holds with a synced commit.
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
