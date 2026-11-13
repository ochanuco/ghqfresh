package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// State remembers, per repository, the remote push time it was last brought up to
// date against, so a --changed run fetches only repositories pushed since.
type State struct {
	LastFull time.Time            `json:"last_full"`
	Synced   map[string]time.Time `json:"synced"`
}

func loadState(path string) (*State, error) {
	s := &State{Synced: map[string]time.Time{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, s); err != nil {
		// A corrupt state only costs one full sweep.
		return &State{Synced: map[string]time.Time{}}, nil
	}
	if s.Synced == nil {
		s.Synced = map[string]time.Time{}
	}
	return s, nil
}

func (s *State) save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// due picks the local repositories to refresh. A full run takes all of them; a
// changed-only run takes those pushed since they were last synced, plus those
// GitHub did not report (renamed, transferred), which only a full run visits.
func due(local []string, remote map[string]RemoteRepo, st *State, full bool) []string {
	var out []string
	for _, name := range local {
		r, known := remote[name]
		switch {
		case full:
			out = append(out, name)
		case !known:
		case r.PushedAt.After(st.Synced[name]):
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
