package scrape

import (
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// benEncode writes a decoded value back as canonical bencode (sorted keys). The
// package has no encoder; this one is for the round-trip check only.
func benEncode(sb *strings.Builder, v any) {
	switch x := v.(type) {
	case int64:
		sb.WriteString("i" + strconv.FormatInt(x, 10) + "e")
	case []byte:
		sb.WriteString(strconv.Itoa(len(x)) + ":")
		sb.Write(x)
	case []any:
		sb.WriteByte('l')
		for _, e := range x {
			benEncode(sb, e)
		}
		sb.WriteByte('e')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		sb.WriteByte('d')
		for _, k := range keys {
			benEncode(sb, []byte(k))
			benEncode(sb, x[k])
		}
		sb.WriteByte('e')
	default:
		panic("benEncode: unexpected type")
	}
}

// benShape walks a decoded value and reports its container depth and the number of
// values in it, which the decoder's bounds cap.
func benShape(t *testing.T, v any, depth int, items *int, in int) {
	t.Helper()
	*items++
	switch x := v.(type) {
	case int64:
	case []byte:
		if len(x) > in {
			t.Fatalf("string of %d bytes from %d bytes of input", len(x), in)
		}
	case []any:
		if depth > maxDepth {
			t.Fatalf("list at depth %d, bound %d", depth, maxDepth)
		}
		for _, e := range x {
			benShape(t, e, depth+1, items, in)
		}
	case map[string]any:
		if depth > maxDepth {
			t.Fatalf("dict at depth %d, bound %d", depth, maxDepth)
		}
		for k, e := range x {
			if len(k) > in {
				t.Fatalf("key of %d bytes from %d bytes of input", len(k), in)
			}
			benShape(t, e, depth+1, items, in)
		}
	default:
		t.Fatalf("unexpected type %T", v)
	}
}

func FuzzDecodeBencode(f *testing.F) {
	nest := func(n int) string { return strings.Repeat("l", n) + strings.Repeat("e", n) }
	for _, s := range []string{
		// the vectors of TestBencodeBounds and the scrape tests
		"", "i12", "5:ab", "d1:ae", "l", "i1ex", "99999999999:x", "d3:abci1e",
		"d1:ai1e1:bl3:xyzee",
		"d14:failure reason9:not today5:flagsd20:min_request_intervali900eee",
		"<html>", "de",
		"d5:filesd20:" + strings.Repeat("x", 20) + "d8:completei3e10:incompletei4eeee",
		// edge seeds
		nest(maxDepth + 1), nest(maxDepth + 2), // at the bound, one past it
		"4294967295:x", "999999999:x", "-1:x", "-5:abcde", "i-0e", "i-e", "ie", "i--1e",
		"i9223372036854775808e", "5:abc", "3:abc", "0:", "00:", "d1:a",
		"dd1:ai1eei1ee", "li1ei2e", "d1:bi1e1:ai2ee", "d1:ai1e1:ai2ee",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		v, err := decodeBencode(in)
		if err != nil {
			if v != nil {
				t.Fatalf("value %v with error %v", v, err)
			}
			return
		}
		items := 0
		benShape(t, v, 0, &items, len(in))
		if items > maxItems {
			t.Fatalf("%d values, bound %d", items, maxItems)
		}
		// decode, re-encode, decode again: the value survives
		var sb strings.Builder
		benEncode(&sb, v)
		v2, err := decodeBencode([]byte(sb.String()))
		if err != nil {
			t.Fatalf("re-encoded %q does not decode: %v", sb.String(), err)
		}
		if !reflect.DeepEqual(v, v2) {
			t.Fatalf("round trip changed the value: %v vs %v (%q)", v, v2, sb.String())
		}
	})
}
