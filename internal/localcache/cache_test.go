package localcache

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCacheSurvivesRestartAndEvictsLeastRecentlyUsed(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir, 6)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"a", "b"} {
		if err = c.Put(key, []byte("abc")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = c.Get("a"); err != nil {
		t.Fatal(err)
	}
	if err = c.Put("c", []byte("def")); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Get("b"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("did not evict LRU entry")
	}
	reopened, err := Open(dir, 6)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"a", "c"} {
		if raw, err := reopened.Get(key); err != nil || len(raw) != 3 {
			t.Fatal("cache lost on reopen")
		}
	}
	if err = reopened.Delete("a"); err != nil {
		t.Fatal(err)
	}
	if _, err = reopened.Get("a"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("deleted entry survived")
	}
}
func TestCacheBudgetAndAtomicFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "unrelated"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Open(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Put("../../../escape", []byte("abc")); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Get("../../../escape"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("oversized body was cached")
	}
	if err = c.Put("key", []byte("ab")); err != nil {
		t.Fatal(err)
	}
	if err = c.Put("key", []byte("xx")); err != nil {
		t.Fatal(err)
	}
	raw, err := c.Get("key")
	if err != nil || !bytes.Equal(raw, []byte("ab")) {
		t.Fatal("immutable entry overwritten")
	}
	if _, err = Open(dir, 0); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(dir, name("key"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("reduced budget not enforced on reopen")
	}
	if _, err = os.Stat(filepath.Join(dir, "unrelated")); err != nil {
		t.Fatal("removed unrelated file")
	}
}
