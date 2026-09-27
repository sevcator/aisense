package store

import (
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"

	"aisense/internal/vault"
)

// UsageRec tracks one (key, model, day) bucket.
type UsageRec struct {
	Requests  int64 `json:"requests"`
	Errors    int64 `json:"errors"`
	TokensIn  int64 `json:"tokens_in"`
	TokensOut int64 `json:"tokens_out"`
}

// Store persists usage buckets to a JSON file with debounced writes.
type Store struct {
	path  string
	mu    sync.Mutex
	data  map[string]*UsageRec // "keyID|model|YYYY-MM-DD" -> rec
	dirty bool
	stop  chan struct{}
	wg    sync.WaitGroup
}

func New(path string) (*Store, error) {
	s := &Store{path: path, data: map[string]*UsageRec{}, stop: make(chan struct{})}
	raw, err := os.ReadFile(path)
	if err == nil {
		// Usage holds no credentials, so an unreadable file is not fatal: keep
		// it untouched, say so, and start counting again.
		if b, _, openErr := vault.OpenOrPlain(raw); openErr != nil {
			log.Printf("[usage] %s belongs to another account or machine; statistics start empty: %v", path, openErr)
		} else {
			_ = json.Unmarshal(b, &s.data)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	s.wg.Add(1)
	go s.flusher()
	return s, nil
}

func (s *Store) flusher() {
	defer s.wg.Done()
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			s.flush()
			return
		case <-t.C:
			s.flush()
		}
	}
}

func (s *Store) flush() {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return
	}
	plain, err := json.MarshalIndent(s.data, "", "  ")
	s.dirty = false
	s.mu.Unlock()
	if err != nil {
		return
	}
	if err := vault.WriteSealed(s.path, plain); err != nil {
		log.Printf("[usage] could not save %s: %v", s.path, err)
	}
}

func (s *Store) Close() {
	close(s.stop)
	s.wg.Wait()
}

func bucketKey(keyID, model, day string) string { return keyID + "|" + model + "|" + day }

// Add records a completed (or failed) request.
func (s *Store) Add(keyID, model string, tokensIn, tokensOut int64, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	day := time.Now().Format("2006-01-02")
	k := bucketKey(keyID, model, day)
	r := s.data[k]
	if r == nil {
		r = &UsageRec{}
		s.data[k] = r
	}
	r.Requests++
	if !ok {
		r.Errors++
	} else {
		r.TokensIn += tokensIn
		r.TokensOut += tokensOut
	}
	s.dirty = true
}

// DayTokens returns total tokens used by a key today (across models).
func (s *Store) DayTokens(keyID string) (in, out int64) {
	day := time.Now().Format("2006-01-02")
	prefix := keyID + "|"
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, r := range s.data {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix && k[len(k)-len(day):] == day {
			in += r.TokensIn
			out += r.TokensOut
		}
	}
	return
}

// Snapshot returns a deep copy of all records.
func (s *Store) Snapshot() map[string]*UsageRec {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]*UsageRec, len(s.data))
	for k, r := range s.data {
		c := *r
		out[k] = &c
	}
	return out
}
