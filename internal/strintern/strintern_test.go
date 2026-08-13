package strintern

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"unsafe"
)

// sameBacking reports whether two strings share a data pointer, i.e. are literally one copy.
func sameBacking(a, b string) bool {
	return unsafe.StringData(a) == unsafe.StringData(b)
}

func TestStringReturnsOneCopy(t *testing.T) {
	tbl := New()

	// Build distinct strings with identical contents so the compiler can't share them for us.
	a := strings.Join([]string{"produc", "tion"}, "")
	b := strings.Join([]string{"produc", "tion"}, "")
	if sameBacking(a, b) {
		t.Skip("test inputs already share backing memory; nothing to prove")
	}

	if got := tbl.String(a); got != "production" {
		t.Fatalf("String(a) = %q", got)
	}
	if !sameBacking(tbl.String(a), tbl.String(b)) {
		t.Error("equal strings were not folded onto one copy")
	}
	if tbl.Len() != 1 {
		t.Errorf("Len = %d, want 1", tbl.Len())
	}
}

func TestLabelsInternsNamesAndValues(t *testing.T) {
	tbl := New()

	mk := func(pod string) map[string]string {
		return map[string]string{
			strings.Join([]string{"name", "space"}, ""): strings.Join([]string{"produc", "tion"}, ""),
			"pod": pod,
		}
	}
	first := tbl.Labels(mk("pod-a"))
	second := tbl.Labels(mk("pod-b"))

	if first["namespace"] != "production" || second["namespace"] != "production" {
		t.Fatalf("interning changed content: %v / %v", first, second)
	}
	if !sameBacking(first["namespace"], second["namespace"]) {
		t.Error("repeated label value not folded onto one copy")
	}
	// 3 distinct strings: "namespace", "production", "pod" — plus the two unique pod values.
	if got, want := tbl.Len(), 5; got != want {
		t.Errorf("Len = %d, want %d", got, want)
	}
}

func TestLabelsNilAndEmpty(t *testing.T) {
	tbl := New()
	if got := tbl.Labels(nil); got != nil {
		t.Errorf("Labels(nil) = %v, want nil", got)
	}
	got := tbl.Labels(map[string]string{"a": ""})
	if len(got) != 1 || got["a"] != "" {
		t.Errorf("Labels dropped an empty value: %v", got)
	}
}

func TestConcurrentInterning(t *testing.T) {
	tbl := New()
	const goroutines, perGoroutine = 16, 200

	var wg sync.WaitGroup
	results := make([][]string, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			out := make([]string, perGoroutine)
			for i := 0; i < perGoroutine; i++ {
				// Same value set from every goroutine, built fresh each time.
				out[i] = tbl.String(fmt.Sprintf("label-value-%d", i))
			}
			results[g] = out
		}(g)
	}
	wg.Wait()

	if got := tbl.Len(); got != perGoroutine {
		t.Fatalf("Len = %d, want %d (races created duplicate entries)", got, perGoroutine)
	}
	for i := 0; i < perGoroutine; i++ {
		for g := 1; g < goroutines; g++ {
			if !sameBacking(results[0][i], results[g][i]) {
				t.Fatalf("value %d not shared between goroutine 0 and %d", i, g)
			}
		}
	}
}
