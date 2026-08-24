package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// BanEntry is one banned IP, as persisted in the bans file.
type BanEntry struct {
	IP       string    `json:"ip"`
	Name     string    `json:"name"` // last known player name, may be empty
	Reason   string    `json:"reason"`
	BannedAt time.Time `json:"banned_at"`
	BannedBy string    `json:"banned_by"`
}

// banList is the in-memory ban set plus the file it persists to. It has no
// lock of its own; the server's one mutex guards it like everything else.
type banList struct {
	path    string
	entries map[string]BanEntry // keyed by IP string
}

// loadBans reads the bans file. A missing file is an empty list, not an error.
func loadBans(path string) (*banList, error) {
	l := &banList{path: path, entries: make(map[string]BanEntry)}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return l, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read bans %q: %w", path, err)
	}
	var entries []BanEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parse bans %q: %w", path, err)
	}
	for _, e := range entries {
		l.entries[e.IP] = e
	}
	return l, nil
}

// save writes the list atomically: a temp file in the same directory, then a
// rename over the target, so a crash mid-write never leaves a torn file.
func (l *banList) save() error {
	entries := l.list()
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(l.path)
	tmp, err := os.CreateTemp(dir, filepath.Base(l.path)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, l.path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

func (l *banList) get(ip string) (BanEntry, bool) {
	e, ok := l.entries[ip]
	return e, ok
}

func (l *banList) add(e BanEntry) { l.entries[e.IP] = e }

func (l *banList) remove(ip string) bool {
	if _, ok := l.entries[ip]; !ok {
		return false
	}
	delete(l.entries, ip)
	return true
}

// list returns the entries in a stable order (oldest ban first).
func (l *banList) list() []BanEntry {
	entries := make([]BanEntry, 0, len(l.entries))
	for _, e := range l.entries {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].BannedAt.Before(entries[j].BannedAt) })
	return entries
}
