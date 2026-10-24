// Package localcache provides a bounded, disposable disk cache for S3 MIME bodies.
package localcache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type entry struct {
	size int64
	used time.Time
}
type Cache struct {
	mu            sync.Mutex
	dir           string
	budget, bytes int64
	entries       map[string]entry
}

func name(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:]) + ".eml"
}

func Open(dir string, budget int64) (*Cache, error) {
	if budget < 0 {
		return nil, errors.New("negative disk cache budget")
	}
	if err := os.MkdirAll(dir, 0750); err != nil {
		return nil, err
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	c := &Cache{dir: dir, budget: budget, entries: map[string]entry{}}
	for _, f := range files {
		n := f.Name()
		if strings.HasPrefix(n, ".body-") && f.Type().IsRegular() {
			if err := os.Remove(filepath.Join(dir, n)); err != nil {
				return nil, err
			}
			continue
		}
		if !strings.HasSuffix(n, ".eml") || len(n) != 68 || !f.Type().IsRegular() {
			continue
		}
		if _, err := hex.DecodeString(n[:64]); err != nil {
			continue
		}
		info, err := f.Info()
		if err != nil {
			return nil, err
		}
		c.entries[n] = entry{size: info.Size(), used: info.ModTime()}
		c.bytes += info.Size()
	}
	if err := c.evict(0); err != nil {
		return nil, err
	}
	return c, nil
}
func (c *Cache) Get(key string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := name(key)
	e, ok := c.entries[n]
	if !ok {
		return nil, os.ErrNotExist
	}
	raw, err := os.ReadFile(filepath.Join(c.dir, n))
	if errors.Is(err, os.ErrNotExist) {
		delete(c.entries, n)
		c.bytes -= e.size
	}
	if err != nil {
		return nil, err
	}
	e.used = time.Now()
	c.entries[n] = e
	return raw, nil
}
func (c *Cache) Put(key string, raw []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := name(key)
	if _, ok := c.entries[n]; ok {
		return nil
	}
	if c.budget == 0 || int64(len(raw)) > c.budget {
		return nil
	}
	if err := c.evict(int64(len(raw))); err != nil {
		return err
	}
	f, err := os.CreateTemp(c.dir, ".body-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0640); err == nil {
		_, err = f.Write(raw)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmp, filepath.Join(c.dir, n)); err != nil {
		return err
	}
	c.entries[n] = entry{size: int64(len(raw)), used: time.Now()}
	c.bytes += int64(len(raw))
	return nil
}
func (c *Cache) Delete(key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := name(key)
	e, ok := c.entries[n]
	if !ok {
		return nil
	}
	if err := os.Remove(filepath.Join(c.dir, n)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	delete(c.entries, n)
	c.bytes -= e.size
	return nil
}
func (c *Cache) evict(extra int64) error {
	if c.bytes+extra <= c.budget {
		return nil
	}
	names := make([]string, 0, len(c.entries))
	for n := range c.entries {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return c.entries[names[i]].used.Before(c.entries[names[j]].used) })
	for _, n := range names {
		if err := os.Remove(filepath.Join(c.dir, n)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		c.bytes -= c.entries[n].size
		delete(c.entries, n)
		if c.bytes+extra <= c.budget {
			break
		}
	}
	return nil
}

type Stats struct {
	Entries       int
	Bytes, Budget int64
}

func (c *Cache) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Stats{Entries: len(c.entries), Bytes: c.bytes, Budget: c.budget}
}
func (c *Cache) Has(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.entries[name(key)]
	return ok
}
