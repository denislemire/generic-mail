package blob

import (
	"context"
	"strings"
	"sync"
	"time"
)

type memObj struct {
	body []byte
	mod  time.Time
	etag string
}

// Mem is an in-memory Store for tests.
type Mem struct {
	mu   sync.Mutex
	objs map[string]*memObj
	seq  int
}

// NewMem returns an empty store.
func NewMem() *Mem {
	return &Mem{objs: make(map[string]*memObj)}
}

func (m *Mem) PutMeta(key string, body []byte, mod time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	m.objs[key] = &memObj{body: append([]byte(nil), body...), mod: mod, etag: itoa(m.seq)}
}

func (m *Mem) List(_ context.Context, prefix string) ([]Meta, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Meta
	for k, o := range m.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, Meta{Key: k, Size: int64(len(o.body)), LastModified: o.mod})
		}
	}
	return out, nil
}

func (m *Mem) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objs[key]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte(nil), o.body...), nil
}

func (m *Mem) Put(_ context.Context, key string, body []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	m.objs[key] = &memObj{body: append([]byte(nil), body...), mod: time.Now().UTC(), etag: itoa(m.seq)}
	return nil
}

func (m *Mem) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objs, key)
	return nil
}

func (m *Mem) GetState(ctx context.Context, key string) ([]byte, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objs[key]
	if !ok {
		return nil, "", ErrNotFound
	}
	return append([]byte(nil), o.body...), o.etag, nil
}

func (m *Mem) PutState(ctx context.Context, key string, body []byte, etag string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.objs[key]; ok {
		if etag != "" && cur.etag != etag {
			return "", ErrPrecondition
		}
	} else if etag != "" {
		return "", ErrPrecondition
	}
	m.seq++
	next := itoa(m.seq)
	m.objs[key] = &memObj{body: append([]byte(nil), body...), mod: time.Now().UTC(), etag: next}
	return next, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
