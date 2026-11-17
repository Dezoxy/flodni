package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// A stored value outside a slider's range must never be clamped by the browser,
// because Apply would then write the clamped value back. Every range input that is
// set from stored config goes through loadRange(), which widens the range first.
func TestPanelRangeInputsLoadThroughTheWideningHelper(t *testing.T) {
	html := string(settingsHTML)
	if !strings.Contains(html, "function loadRange(") {
		t.Fatal("settings.html has no loadRange() helper")
	}
	inputs := regexp.MustCompile(`<input type="range" name="([^"]+)"`).FindAllStringSubmatch(html, -1)
	if len(inputs) == 0 {
		t.Fatal("no range inputs found")
	}
	for _, m := range inputs {
		name := m[1]
		// a range input's .value is never assigned directly, only through the helper
		direct := regexp.MustCompile(`\[name="` + name + `"\]'?\)?\.value\s*=`)
		if direct.MatchString(html) {
			t.Errorf("range input %q has .value assigned directly; use loadRange()", name)
		}
	}
	// the generic engine loader routes range inputs through the helper
	if !strings.Contains(html, "loadRange(input,") {
		t.Error("the engine config loader does not call loadRange() for range inputs")
	}
	// the core loader calls it for both of its sliders
	if !strings.Contains(html, "loadRange(csSlider,") {
		t.Error("loadCore() does not load cacheSize through loadRange()")
	}
	if !strings.Contains(html, `loadRange(form.querySelector('[name="readerReadAHead"]'),`) {
		t.Error("loadCore() does not load readerReadAHead through loadRange()")
	}
}

func TestPanelCacheSizeSliderReaches2048(t *testing.T) {
	html := string(settingsHTML)
	for name, floor := range map[string]int{"cacheSize": 2048, "read_ahead_budget_mb": 2048} {
		m := regexp.MustCompile(`<input type="range" name="` + name + `" min="(\d+)" max="(\d+)"`).FindStringSubmatch(html)
		if m == nil {
			t.Fatalf("range input %q not found", name)
		}
		if max, _ := strconv.Atoi(m[2]); max < floor {
			t.Errorf("%s max is %d, want at least %d", name, max, floor)
		}
	}
}

// Apply writes the loaded CacheSize byte-for-byte when the slider was not moved.
func TestPanelUnmovedCacheSliderKeepsStoredBytes(t *testing.T) {
	html := string(settingsHTML)
	if !regexp.MustCompile(`\? originalCore\.CacheSize\s*:`).MatchString(html) {
		t.Error("an unmoved cacheSize slider must keep originalCore.CacheSize as stored")
	}
}

// Behaviour check of loadRange() in isolation, when node is installed.
func TestPanelLoadRangeWidensAndKeepsTheValue(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; the static checks above still run")
	}
	html := string(settingsHTML)
	i := strings.Index(html, "function loadRange(")
	if i < 0 {
		t.Fatal("loadRange() not found")
	}
	// the helper ends at the first line that closes it at the same indent
	j := strings.Index(html[i:], "\n        }\n")
	if j < 0 {
		t.Fatal("end of loadRange() not found")
	}
	src := html[i : i+j+len("\n        }\n")]
	script := src + `
function fake(min, max, step, name) {
  const note = { textContent: '', hidden: true, className: '' };
  const label = { textContent: '' };
  const parent = { querySelector: s => s === '.range-val' ? label : null, appendChild() {} };
  const s = { name, min: String(min), max: String(max), step: String(step), value: '', parentElement: parent,
    ownerDocument: { createElement: () => note } };
  parent.querySelector = sel => sel === '.range-val' ? label : sel === '.range-note' ? note : null;
  return { s, label, note };
}
const assert = require('assert');
let f = fake(64, 256, 16, 'cacheSize');
loadRange(f.s, 1024, ' MB');
assert.strictEqual(Number(f.s.max), 1024); assert.strictEqual(f.s.value, '1024'); assert.strictEqual(f.label.textContent, '1024 MB');
assert.ok(f.note.textContent.includes('kept as configured'));
f = fake(64, 256, 16, 'cacheSize');
loadRange(f.s, 32, ' MB');
assert.strictEqual(Number(f.s.min), 32); assert.strictEqual(f.s.value, '32');
f = fake(64, 256, 16, 'cacheSize');
loadRange(f.s, 1000, ' MB');   // above max and off the 16 MB grid
assert.strictEqual(f.s.value, '1000'); assert.strictEqual(f.label.textContent, '1000 MB');
f = fake(64, 2048, 16, 'cacheSize');
loadRange(f.s, 512, ' MB');
assert.strictEqual(Number(f.s.max), 2048); assert.strictEqual(f.s.value, '512'); assert.strictEqual(f.note.textContent, '');
`
	dir := t.TempDir()
	file := filepath.Join(dir, "range.js")
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, file).CombinedOutput(); err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
