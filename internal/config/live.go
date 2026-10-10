package config

import (
	"crypto/sha256"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Snapshot is one loaded config with everything derived from it: the
// resolved timezone and the config_version it was published under. Readers
// load the pointer once per request or tick and use that snapshot
// throughout (§5.5) — the same discipline as reading now once — so a
// request never resolves a preset from one config and reports the
// config_version of the next.
type Snapshot struct {
	*Config
	Location *time.Location
	Version  int

	fullHash   [32]byte
	clientHash [32]byte
}

// Live publishes the current config to the HTTP handlers and the sweep
// through one atomic.Pointer, and replaces it on SIGHUP (§5.5).
type Live struct {
	path        string
	vapidPublic string
	current     atomic.Pointer[Snapshot]
	reloading   sync.Mutex // one reload at a time; readers never wait
}

// NewLive publishes cfg, already loaded and validated at boot, as
// config_version 1. The counter lives in memory and restarts at 1, so it
// can move backwards across a restart; clients compare with !=.
func NewLive(path string, cfg *Config, vapidPublic string) *Live {
	l := &Live{path: path, vapidPublic: vapidPublic}
	l.current.Store(l.snapshot(cfg, 1))
	return l
}

// Load returns the current snapshot.
func (l *Live) Load() *Snapshot { return l.current.Load() }

// Reload re-reads and re-validates the file through the same code path as
// boot and config check. On any failure — a validation error, or a file
// gone missing or unreadable — it logs and keeps the running config: a
// failed reload never stores a pointer, so "keep the old config" is a
// skipped assignment rather than a rollback, and the default is never
// rewritten (§5.5).
//
// A successful load always replaces the config. Two hashes answer the two
// questions (§5.5): the full hash, over every resolved value, decides only
// what is logged; the client hash, over exactly what /api/config returns,
// decides whether config_version moves. One hash cannot do both: the client
// projection is blind to every edit that changes what a chip does, and
// bumping on every load would reload every open page on a HUP sent twice.
func (l *Live) Reload(log *slog.Logger) {
	l.reloading.Lock()
	defer l.reloading.Unlock()

	cfg, err := Check(l.path)
	if err != nil {
		log.Error("config reload failed, keeping the running config", "error", err)
		return
	}
	old := l.current.Load()
	version := old.Version
	next := l.snapshot(cfg, version)
	if next.clientHash != old.clientHash {
		version++
		next.Version = version
	}
	l.current.Store(next)
	if next.fullHash == old.fullHash {
		log.Info("config unchanged", "path", l.path)
		return
	}
	log.Info("config reloaded", "path", l.path, "presets", len(cfg.Presets), "config_version", version)
}

func (l *Live) snapshot(cfg *Config, version int) *Snapshot {
	loc, err := time.LoadLocation(cfg.General.Timezone)
	if err != nil {
		loc = time.UTC // unreachable: validation already loaded it
	}
	return &Snapshot{
		Config:     cfg,
		Location:   loc,
		Version:    version,
		fullHash:   hashJSON(cfg),
		clientHash: hashJSON(l.ClientView(cfg)),
	}
}

// ClientView is the /api/config projection minus config_version — the
// presets as {key,label,quick} in file order, never kind, offset, at, days,
// weekday, same_day_ok, the timezone or retention_days (§8.2). It is both
// the response body and the input to the client hash, so the two cannot
// disagree about what the client sees.
func (l *Live) ClientView(cfg *Config) ClientView {
	v := ClientView{
		VAPIDPublic:   l.vapidPublic,
		DefaultPreset: cfg.General.DefaultPreset,
		Presets:       make([]ClientPreset, 0, len(cfg.Presets)),
		Picker:        cfg.Picker,
	}
	for _, p := range cfg.Presets {
		v.Presets = append(v.Presets, ClientPreset{Key: p.Key, Label: p.Label, Quick: p.Quick})
	}
	return v
}

type ClientView struct {
	VAPIDPublic   string         `json:"vapid_public"`
	DefaultPreset string         `json:"default_preset"`
	Presets       []ClientPreset `json:"presets"`
	Picker        Picker         `json:"picker"`
}

type ClientPreset struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Quick bool   `json:"quick"`
}

// hashJSON hashes the JSON encoding of v. Struct fields encode in
// declaration order and presets in file order, so equal configs hash
// equal and nothing depends on map iteration.
func hashJSON(v any) [32]byte {
	b, _ := json.Marshal(v) // plain structs of strings, ints and bools
	return sha256.Sum256(b)
}
