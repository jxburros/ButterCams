package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/jxburros/buttercams/internal/config/migrations"
	"github.com/jxburros/buttercams/internal/winsvc"
)

// FileName is the configuration file inside the data root.
const FileName = "config.json"

// Backups is how many previous versions are kept (config.json.1 … .10).
const Backups = 10

// Store reads and writes config.json. Only the guardian uses it (spec 4.1).
type Store struct {
	mu   sync.Mutex
	path string
	env  Env
}

// NewStore returns a store for <dataRoot>/config.json.
func NewStore(dataRoot string, env Env) *Store {
	return &Store{path: filepath.Join(dataRoot, FileName), env: env}
}

// Path returns the location of config.json.
func (s *Store) Path() string { return s.path }

// LoadResult describes how a configuration was obtained.
type LoadResult struct {
	Config *Config
	// Created is true when no configuration existed and defaults were used.
	Created bool
	// RecoveredFrom is the backup file that was loaded because config.json was
	// unreadable; empty when config.json itself was good.
	RecoveredFrom string
	// Problem is why config.json could not be used.
	Problem string
	// MigratedFrom is the schema version found on disk when it was older.
	MigratedFrom int
}

// Parse decodes, migrates and validates a config document.
func Parse(data []byte, env Env) (cfg *Config, migratedFrom int, err error) {
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, 0, fmt.Errorf("not valid JSON: %w", err)
	}
	from, err := migrations.Run(doc, SchemaVersion)
	if err != nil {
		return nil, 0, err
	}
	if from != SchemaVersion {
		migratedFrom = from
		if data, err = json.Marshal(doc); err != nil {
			return nil, 0, err
		}
	}
	cfg = &Config{}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, 0, fmt.Errorf("unexpected structure: %w", err)
	}
	cfg.Normalize()
	if err := Validate(cfg, env); err != nil {
		return nil, 0, err
	}
	return cfg, migratedFrom, nil
}

func (s *Store) readFile(path string) (*Config, int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	return Parse(data, s.env)
}

func (s *Store) backupPath(n int) string {
	return fmt.Sprintf("%s.%d", s.path, n)
}

// Load reads config.json. When it is missing, defaults are returned (and not
// yet saved). When it is unreadable, the newest readable backup is loaded and
// reported through LoadResult so the guardian can raise a Critical status
// (spec 5.3).
func (s *Store) Load(defaultStaging string) (LoadResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cfg, from, err := s.readFile(s.path)
	if err == nil {
		return LoadResult{Config: cfg, MigratedFrom: from}, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		// A missing file with backups present means the file was lost.
		for n := 1; n <= Backups; n++ {
			if cfg, from, berr := s.readFile(s.backupPath(n)); berr == nil {
				return LoadResult{Config: cfg, MigratedFrom: from, RecoveredFrom: filepath.Base(s.backupPath(n)),
					Problem: "config.json is missing"}, nil
			}
		}
		return LoadResult{Config: Default(defaultStaging), Created: true}, nil
	}
	problem := err.Error()
	for n := 1; n <= Backups; n++ {
		if cfg, from, berr := s.readFile(s.backupPath(n)); berr == nil {
			return LoadResult{Config: cfg, MigratedFrom: from, RecoveredFrom: filepath.Base(s.backupPath(n)),
				Problem: problem}, nil
		}
	}
	return LoadResult{}, fmt.Errorf("config.json is unreadable (%s) and no backup could be loaded", problem)
}

// Save validates cfg and writes it atomically, keeping the previous ten
// versions as config.json.1 … config.json.10.
func (s *Store) Save(cfg *Config) error {
	cfg.Normalize()
	if err := Validate(cfg, s.env); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := writeFileSync(tmp, data); err != nil {
		return fmt.Errorf("writing %s: %w", tmp, err)
	}
	s.rotateBackups()
	if err := winsvc.ReplaceFile(tmp, s.path); err != nil {
		return fmt.Errorf("replacing %s: %w", s.path, err)
	}
	return nil
}

// rotateBackups shifts .1 → .2 … .9 → .10 and copies the current file to .1.
// A failure here never blocks saving the new configuration.
func (s *Store) rotateBackups() {
	if _, err := os.Stat(s.path); err != nil {
		return
	}
	os.Remove(s.backupPath(Backups))
	for n := Backups - 1; n >= 1; n-- {
		os.Rename(s.backupPath(n), s.backupPath(n+1))
	}
	if data, err := os.ReadFile(s.path); err == nil {
		writeFileSync(s.backupPath(1), data)
	}
}

func writeFileSync(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// WriteFileAtomic writes data to path through a temporary file and an atomic
// replace. Other small state files in the data root use it too.
func WriteFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := writeFileSync(tmp, data); err != nil {
		return err
	}
	return winsvc.ReplaceFile(tmp, path)
}
