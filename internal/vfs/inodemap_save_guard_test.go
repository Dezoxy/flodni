package vfs

import (
	"bytes"
	"errors"
	"flodni/internal/flog"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"flodni/internal/metadb"
)

func guardDB(t *testing.T) *metadb.DB {
	t.Helper()
	db, err := metadb.New(filepath.Join(t.TempDir(), "s.db"), flog.NewText(io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// seedStored writes n file rows and one dir row straight into the state DB.
func seedStored(t *testing.T, db *metadb.DB, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		path := filepath.Join("/lib/movies", string(rune('a'+i))+".mkv")
		if err := db.UpsertFile(path, strings.Repeat("a", 40), i, uint64(100+i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.UpsertDir("/movies", GenerateDirInode("/movies")); err != nil {
		t.Fatal(err)
	}
}

// storedRows returns every stored inode row as "type|path", sorted by the query.
func storedRows(t *testing.T, db *metadb.DB) []string {
	t.Helper()
	rows, err := db.SQL().Query("SELECT type, full_path FROM inodes ORDER BY type, full_path")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var typ, path string
		if err := rows.Scan(&typ, &path); err != nil {
			t.Fatal(err)
		}
		out = append(out, typ+"|"+path)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func newGuardMap(t *testing.T, buf io.Writer) *InodeMap {
	t.Helper()
	return NewInodeMap(filepath.Join(t.TempDir(), "inode_map.json"), flog.NewText(buf))
}

func mustRefuse(t *testing.T, im *InodeMap) {
	t.Helper()
	if err := im.SaveToDisk(); !errors.Is(err, ErrSaveRefused) {
		t.Fatalf("SaveToDisk = %v, want ErrSaveRefused", err)
	}
}

func TestSaveRefusedAfterFailedLoad(t *testing.T) {
	db := guardDB(t)
	seedStored(t, db, 3)
	before := storedRows(t, db)

	im := newGuardMap(t, io.Discard)
	// Boot order: the JSON load runs before SetDB and fails on a corrupt file.
	if err := os.WriteFile(im.savePath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := im.LoadFromDisk(); err == nil {
		t.Fatal("load of a corrupt file succeeded")
	}
	im.SetDB(db)
	im.AddFile("/lib/movies/new.mkv", strings.Repeat("b", 40), 0) // non-empty: only the load failure can refuse

	mustRefuse(t, im)

	if got := storedRows(t, db); !reflect.DeepEqual(got, before) {
		t.Fatalf("rows changed after a refused save:\n got %v\nwant %v", got, before)
	}
	if !im.IsDirty() {
		t.Fatal("a refused save cleared the dirty flag")
	}
}

func TestSaveAllowedAfterFailedLoadThenGoodLoad(t *testing.T) {
	db := guardDB(t)
	seedStored(t, db, 3)

	im := newGuardMap(t, io.Discard)
	if err := os.WriteFile(im.savePath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := im.LoadFromDisk(); err == nil {
		t.Fatal("load of a corrupt file succeeded")
	}
	im.SetDB(db)
	if err := im.LoadFromDisk(); err != nil { // the reload from the DB
		t.Fatal(err)
	}
	im.AddFile("/lib/movies/new.mkv", strings.Repeat("b", 40), 0)

	if err := im.SaveToDisk(); err != nil {
		t.Fatalf("SaveToDisk after a good load: %v", err)
	}
	found := false
	for _, r := range storedRows(t, db) {
		if r == "file|/lib/movies/new.mkv" {
			found = true
		}
	}
	if !found {
		t.Fatal("new file row was not saved")
	}
}

func TestSaveRefusesEmptyMapOverStoredFiles(t *testing.T) {
	db := guardDB(t)
	seedStored(t, db, 3)
	before := storedRows(t, db)

	im := newGuardMap(t, io.Discard)
	im.SetDB(db) // no load: the map is empty, as after a silent partial load

	mustRefuse(t, im)

	if got := storedRows(t, db); !reflect.DeepEqual(got, before) {
		t.Fatalf("rows changed after a refused save:\n got %v\nwant %v", got, before)
	}
}

func TestSaveRefusesDirsOnlyMapOverStoredFiles(t *testing.T) {
	db := guardDB(t)
	seedStored(t, db, 3)
	before := storedRows(t, db)

	im := newGuardMap(t, io.Discard)
	im.SetDB(db)
	im.AddDir("/series")
	im.AddDir("/series/show")

	mustRefuse(t, im)

	if got := storedRows(t, db); !reflect.DeepEqual(got, before) {
		t.Fatalf("rows changed after a refused save:\n got %v\nwant %v", got, before)
	}
}

func TestFirstBootSavesAnEmptyMap(t *testing.T) {
	db := guardDB(t)
	im := newGuardMap(t, io.Discard)
	// Nothing stored anywhere: a missing JSON file and an empty DB are not failures.
	if err := im.LoadFromDisk(); err != nil {
		t.Fatalf("load with no JSON file: %v", err)
	}
	im.SetDB(db)
	if err := im.LoadFromDisk(); err != nil {
		t.Fatalf("load of an empty DB: %v", err)
	}

	if err := im.SaveToDisk(); err != nil {
		t.Fatalf("first-boot save of an empty map: %v", err)
	}
	im.AddFile("/lib/movies/a.mkv", strings.Repeat("a", 40), 0)
	if err := im.SaveToDisk(); err != nil {
		t.Fatalf("first-boot save with one file: %v", err)
	}
	if got := storedRows(t, db); !reflect.DeepEqual(got, []string{"dir|/", "file|/lib/movies/a.mkv"}) {
		t.Fatalf("rows = %v, want the root dir and the one file", got)
	}
}

func TestSaveAllowsLargeShrinkToOneFile(t *testing.T) {
	db := guardDB(t)
	seedStored(t, db, 20)

	im := newGuardMap(t, io.Discard)
	im.SetDB(db)
	if err := im.LoadFromDisk(); err != nil {
		t.Fatal(err)
	}
	keep := "/lib/movies/a.mkv"
	for i := 1; i < 20; i++ {
		im.RemoveFile(filepath.Join("/lib/movies", string(rune('a'+i))+".mkv"))
	}

	if err := im.SaveToDisk(); err != nil {
		t.Fatalf("a shrink that leaves one file was refused: %v", err)
	}
	var files []string
	for _, r := range storedRows(t, db) {
		if strings.HasPrefix(r, "file|") {
			files = append(files, r)
		}
	}
	if want := []string{"file|" + keep}; !reflect.DeepEqual(files, want) {
		t.Fatalf("stored files = %v, want %v", files, want)
	}
}

func TestRefusalIsLoggedOncePerStreak(t *testing.T) {
	db := guardDB(t)
	seedStored(t, db, 2)

	var buf bytes.Buffer
	im := newGuardMap(t, &buf)
	im.SetDB(db)
	im.AddDir("/series") // dirty, as the background saver requires

	for i := 0; i < 3; i++ {
		mustRefuse(t, im)
	}
	im.Stop() // saves again because the map is still dirty

	out := buf.String()
	if got := strings.Count(out, "save refused"); got != 1 {
		t.Fatalf("%d refusal lines, want 1:\n%s", got, out)
	}
	if strings.Contains(out, "save failed") {
		t.Fatalf("a refusal was also logged as a failure:\n%s", out)
	}

	// A good load ends the streak: the next refusal logs again.
	if err := im.LoadFromDisk(); err != nil {
		t.Fatal(err)
	}
	im.RemoveFile("/lib/movies/a.mkv")
	im.RemoveFile("/lib/movies/b.mkv")
	buf.Reset()
	mustRefuse(t, im)
	if got := strings.Count(buf.String(), "save refused"); got != 1 {
		t.Fatalf("%d refusal lines after a new streak, want 1:\n%s", got, buf.String())
	}
}
