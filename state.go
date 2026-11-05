package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// PostRef is the Bluesky record a mirrored tweet became, plus its thread root.
type PostRef struct {
	URI     string `json:"uri"`
	CID     string `json:"cid"`
	RootURI string `json:"root_uri"`
	RootCID string `json:"root_cid"`
}

// State is persisted as JSON on a Docker volume so restarts never double-post.
type State struct {
	mu   sync.Mutex
	path string

	XUserID  string             `json:"x_user_id"`
	SinceID  string             `json:"since_id"`
	Mirrored map[string]PostRef `json:"mirrored"` // tweet ID -> bluesky post
	Failures map[string]int     `json:"failures"` // tweet ID -> attempt count
}

const maxMirroredEntries = 1000

func LoadState(path string) (*State, error) {
	s := &State{path: path, Mirrored: map[string]PostRef{}, Failures: map[string]int{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, err
	}
	if s.Mirrored == nil {
		s.Mirrored = map[string]PostRef{}
	}
	if s.Failures == nil {
		s.Failures = map[string]int{}
	}
	return s, nil
}

// Save writes atomically (temp file + rename).
func (s *State) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()

	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// prune keeps the newest N mirrored entries (tweet IDs are time-ordered snowflakes).
func (s *State) prune() {
	if len(s.Mirrored) <= maxMirroredEntries {
		return
	}
	ids := make([]string, 0, len(s.Mirrored))
	for id := range s.Mirrored {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return idLess(ids[i], ids[j]) })
	for _, id := range ids[:len(ids)-maxMirroredEntries] {
		delete(s.Mirrored, id)
	}
}

// idLess compares numeric snowflake IDs given as strings.
func idLess(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}
