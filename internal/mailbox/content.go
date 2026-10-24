package mailbox

import (
	"context"
	"crypto/sha256"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/denislemire/generic-mail/internal/state"
)

func (e *Engine) bodyLoadLock(loc, key string) *sync.Mutex {
	sum := sha256.Sum256([]byte(loc + "\n" + key))
	return &e.bodyLoads[int(sum[0])%len(e.bodyLoads)]
}
func (e *Engine) memoryBody(loc, key string) ([]byte, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	raw, ok := e.cache[loc+"\n"+key]
	if !ok {
		return nil, false
	}
	e.memoryHits.Add(1)
	return append([]byte(nil), raw...), true
}

// Body checks RAM and persistent local storage before S3. A bounded set of
// striped locks coalesces concurrent cold loads without unbounded per-key state.
func (e *Engine) Body(ctx context.Context, loc, key string) ([]byte, error) {
	if raw, ok := e.memoryBody(loc, key); ok {
		return raw, nil
	}
	load := e.bodyLoadLock(loc, key)
	load.Lock()
	defer load.Unlock()
	if raw, ok := e.memoryBody(loc, key); ok {
		return raw, nil
	}
	var raw []byte
	var err error
	if loc == locFile {
		raw, err = os.ReadFile(filepath.Join(e.files, key))
	} else {
		raw, err = e.disk.Get(loc + "\n" + key)
		if err != nil {
			e.s3Reads.Add(1)
			raw, err = e.blobs.Get(ctx, key)
		} else {
			e.diskHits.Add(1)
		}
	}
	if err != nil {
		return nil, err
	}
	e.cacheContentLocked(ctx, loc, key, raw)
	return raw, nil
}
func (e *Engine) cacheContent(ctx context.Context, loc, key string, raw []byte) {
	load := e.bodyLoadLock(loc, key)
	load.Lock()
	defer load.Unlock()
	e.cacheContentLocked(ctx, loc, key, raw)
}

// The load stripe is held; never call while holding the mailbox mutex. Derived
// data is rebuildable: failures must not fail a committed mail delivery.
func (e *Engine) cacheContentLocked(ctx context.Context, loc, key string, raw []byte) {
	e.mu.Lock()
	referenced := false
	for _, b := range e.boxes {
		for _, m := range b.msgs {
			if m.loc == loc && m.key == key {
				referenced = true
				break
			}
		}
		if referenced {
			break
		}
	}
	if referenced {
		e.rememberLocked(loc, key, raw)
	}
	e.mu.Unlock()
	if !referenced {
		return
	}
	if loc == locS3 {
		if err := e.disk.Put(loc+"\n"+key, raw); err != nil {
			log.Printf("local body cache write failed; S3 fallback remains available")
		}
	}
	if err := e.db.IndexContent(ctx, state.ContentKey{Loc: loc, Ref: key}, raw); err != nil {
		log.Printf("local search indexing deferred")
	}
}

// WarmContent backfills missing derived data with bounded concurrency. Existing
// index rows skip remote downloads on restart, including intentionally evicted
// or oversized bodies. Active reads can refill those from S3 when needed.
func (e *Engine) WarmContent(ctx context.Context) {
	e.mu.Lock()
	keys := map[state.ContentKey]int64{}
	for _, b := range e.boxes {
		for _, m := range b.msgs {
			keys[state.ContentKey{Loc: m.loc, Ref: m.key}] = m.size
		}
	}
	e.mu.Unlock()
	jobs := make(chan state.ContentKey)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for key := range jobs {
				indexed, err := e.db.ContentIndexed(ctx, key)
				if err != nil {
					continue
				}
				if indexed {
					stats := e.disk.Stats()
					if key.Loc == locFile || e.disk.Has(key.Loc+"\n"+key.Ref) || stats.Budget == 0 || keys[key] > stats.Budget-stats.Bytes {
						continue
					}
				}
				raw, err := e.Body(ctx, key.Loc, key.Ref)
				if err != nil {
					if ctx.Err() == nil {
						log.Printf("local content warmup deferred")
					}
					continue
				}
				// Body may have returned an already warm RAM entry seeded by ingestion.
				e.cacheContent(ctx, key.Loc, key.Ref, raw)
			}
		}()
	}
	for key := range keys {
		select {
		case jobs <- key:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return
		}
	}
	close(jobs)
	wg.Wait()
	if ctx.Err() == nil {
		stats := e.disk.Stats()
		complete, fallback, err := e.db.SearchStats(ctx)
		if err == nil {
			log.Printf("local content warmup complete: checked=%d cached=%d cache_bytes=%d indexed=%d scan_fallback=%d", len(keys), stats.Entries, stats.Bytes, complete, fallback)
		}
	}
}

func (e *Engine) SearchCandidates(ctx context.Context, query string) (map[state.ContentKey]bool, error) {
	return e.db.SearchCandidates(ctx, query)
}

// ContentStats contains counts only; no message identifiers or content.
type ContentStats struct {
	MemoryHits, DiskHits, S3Reads uint64
	DiskEntries                   int
	DiskBytes                     int64
}

func (e *Engine) ContentStats() ContentStats {
	disk := e.disk.Stats()
	return ContentStats{MemoryHits: e.memoryHits.Load(), DiskHits: e.diskHits.Load(), S3Reads: e.s3Reads.Load(), DiskEntries: disk.Entries, DiskBytes: disk.Bytes}
}
