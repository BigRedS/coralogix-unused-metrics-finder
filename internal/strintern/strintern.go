// Package strintern deduplicates the strings retained by a large metrics catalog.
//
// encoding/json allocates a fresh string for every key and value it decodes, so a catalog of
// N series holds N copies of "namespace", N copies of "production", and so on. Label names
// and most label values repeat across nearly every series, so folding them onto one copy each
// cuts the catalog's retained heap substantially — the dominant cost at millions of series.
package strintern

import (
	"hash/maphash"
	"sync"
)

// shardCount is a power of two so the shard index is a mask, not a division. 64 shards keeps
// lock contention low with the scan's default worker count while staying cheap to allocate.
const shardCount = 64

type shard struct {
	mu sync.RWMutex
	m  map[string]string
}

// Table maps equal strings onto a single retained copy. Safe for concurrent use. A Table keeps
// every string handed to it alive, so use one per scan and drop it when the catalog is released.
type Table struct {
	seed   maphash.Seed
	shards [shardCount]shard
}

func New() *Table {
	t := &Table{seed: maphash.MakeSeed()}
	for i := range t.shards {
		t.shards[i].m = make(map[string]string)
	}
	return t
}

// String returns the canonical copy of s, storing s as the canonical copy if it is the first
// of its value to arrive.
func (t *Table) String(s string) string {
	// Empty strings are already free; skip the hashing and locking.
	if s == "" {
		return ""
	}
	sh := &t.shards[maphash.String(t.seed, s)&(shardCount-1)]

	sh.mu.RLock()
	canonical, ok := sh.m[s]
	sh.mu.RUnlock()
	if ok {
		return canonical
	}

	sh.mu.Lock()
	defer sh.mu.Unlock()
	// Another goroutine may have interned it between the RUnlock and the Lock.
	if canonical, ok := sh.m[s]; ok {
		return canonical
	}
	sh.m[s] = s
	return s
}

// Labels returns a copy of m with every name and value interned. The returned map is allocated
// at exactly len(m), which also sheds the spare bucket capacity a json-decoded map carries.
func (t *Table) Labels(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[t.String(k)] = t.String(v)
	}
	return out
}

// Len reports how many distinct strings are retained — a useful scale signal when diagnosing
// catalog memory use.
func (t *Table) Len() int {
	n := 0
	for i := range t.shards {
		t.shards[i].mu.RLock()
		n += len(t.shards[i].m)
		t.shards[i].mu.RUnlock()
	}
	return n
}
