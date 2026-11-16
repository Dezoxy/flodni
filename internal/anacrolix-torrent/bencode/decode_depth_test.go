package bencode

import (
	"os"
	"runtime/debug"
	"strings"
	"testing"
)

// The decoder recurses once per nested list or dict. An input of a few hundred KB of "l"
// overflowed the goroutine stack, which is a fatal, unrecoverable error: a deep input must come
// back as an error instead. The tests shrink the maximum stack so that the unlimited decoder dies
// within the test, and run the nesting well past the limit.

const (
	wantLimit  = 256   // the documented nesting limit
	crashDepth = 50000 // deep enough to overflow the shrunken stack without a limit
)

func smallStack(t *testing.T) {
	t.Helper()
	old := debug.SetMaxStack(1 << 20)
	t.Cleanup(func() { debug.SetMaxStack(old) })
}

func nestedLists(n int) string { return strings.Repeat("l", n) + strings.Repeat("e", n) }

func nestedDicts(n int) string {
	return strings.Repeat("d1:a", n) + "i1e" + strings.Repeat("e", n)
}

type nestedList []nestedList

type nestedStruct struct {
	A *nestedStruct `bencode:"a"`
}

func TestDecodeDepthRefusesDeepInput(t *testing.T) {
	smallStack(t)
	for _, tc := range []struct {
		name string
		in   string
		into func() interface{}
	}{
		{"list into interface", nestedLists(crashDepth), func() interface{} { return new(interface{}) }},
		{"dict into interface", nestedDicts(crashDepth), func() interface{} { return new(interface{}) }},
		{"list into typed slices", nestedLists(crashDepth), func() interface{} { return new(nestedList) }},
		{"dict into typed structs", nestedDicts(crashDepth), func() interface{} { return new(nestedStruct) }},
		{"list into a struct field that is dropped", "d1:z" + nestedLists(crashDepth) + "e", func() interface{} { return new(nestedStruct) }},
		{"list into a raw value", nestedLists(crashDepth), func() interface{} { return new(Bytes) }},
		{"unterminated list", strings.Repeat("l", 200000), func() interface{} { return new(interface{}) }},
		{"unterminated dict", "d1:a" + strings.Repeat("l", 200000), func() interface{} { return new(interface{}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Unmarshal([]byte(tc.in), tc.into())
			if err == nil {
				t.Fatal("a deeply nested value was accepted")
			}
			if !strings.Contains(err.Error(), "nest") {
				t.Fatalf("refused for another reason: %v", err)
			}
		})
	}
}

func TestDecodeDepthLimitIsExact(t *testing.T) {
	for _, tc := range []struct {
		name string
		mk   func(int) string
	}{{"list", nestedLists}, {"dict", nestedDicts}} {
		t.Run(tc.name, func(t *testing.T) {
			var v interface{}
			if err := Unmarshal([]byte(tc.mk(wantLimit)), &v); err != nil {
				t.Fatalf("nesting at the limit refused: %v", err)
			}
			if err := Unmarshal([]byte(tc.mk(wantLimit+1)), &v); err == nil {
				t.Fatal("nesting one past the limit accepted")
			}
		})
	}
}

// A stream decoder is reused after an error: the depth must not leak into the next value.
func TestDecodeDepthResetsBetweenValues(t *testing.T) {
	d := NewDecoder(strings.NewReader(nestedLists(wantLimit+10) + "li1ee"))
	var v interface{}
	if err := d.Decode(&v); err == nil {
		t.Fatal("deep value accepted")
	}
	d = NewDecoder(strings.NewReader("li1ee" + "li2ee"))
	for i := 0; i < 2; i++ {
		if err := d.Decode(&v); err != nil {
			t.Fatalf("value %d: %v", i, err)
		}
	}
}

func TestDecodeDepthRealTorrentsStillDecode(t *testing.T) {
	for _, f := range []string{"archlinux-2011.08.19-netinstall-i686.iso.torrent", "continuum.torrent"} {
		b, err := os.ReadFile("testdata/" + f)
		if err != nil {
			t.Fatal(err)
		}
		var v interface{}
		if err := Unmarshal(b, &v); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
}
