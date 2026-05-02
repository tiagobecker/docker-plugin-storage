package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"
)

type Volume struct {
	Name       string            `json:"name"`
	Mountpoint string            `json:"mountpoint"`
	Opts       map[string]string `json:"opts,omitempty"`
	ProjectID  uint32            `json:"project_id,omitempty"`
	Size       string            `json:"size,omitempty"`
	Inodes     string            `json:"inodes,omitempty"`
	RefCount   int               `json:"ref_count"`
	CreatedAt  time.Time         `json:"created_at"`
	UpdatedAt  time.Time         `json:"updated_at"`
}

type Snapshot struct {
	Name         string    `json:"name"`
	Volume       string    `json:"volume"`
	Path         string    `json:"path"`
	ManifestPath string    `json:"manifest_path,omitempty"`
	Bytes        int64     `json:"bytes"`
	SHA256       string    `json:"sha256,omitempty"`
	Format       string    `json:"format,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

type Database struct {
	NextProjectID uint32               `json:"next_project_id"`
	Volumes       map[string]*Volume   `json:"volumes"`
	Snapshots     map[string]*Snapshot `json:"snapshots"`
}

type Store struct {
	path string
	mu   sync.Mutex
	db   Database
}

func Open(path string) (*Store, error) {
	s := &Store{path: path}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.withFileLock(s.loadLocked)
}

func (s *Store) loadLocked() error {
	s.db = Database{
		NextProjectID: 200000,
		Volumes:       map[string]*Volume{},
		Snapshots:     map[string]*Snapshot{},
	}

	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s.saveLocked()
	}
	if err != nil {
		return err
	}
	if len(b) == 0 {
		return s.saveLocked()
	}
	if err := json.Unmarshal(b, &s.db); err != nil {
		return fmt.Errorf("decode store: %w", err)
	}
	if s.db.Volumes == nil {
		s.db.Volumes = map[string]*Volume{}
	}
	if s.db.Snapshots == nil {
		s.db.Snapshots = map[string]*Snapshot{}
	}
	if s.db.NextProjectID == 0 {
		s.db.NextProjectID = 200000
	}
	return nil
}

func (s *Store) saveLocked() error {
	tmp := s.path + ".tmp"
	b, err := json.MarshalIndent(s.db, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Store) CreateVolume(name, mountpoint string, opts map[string]string) (*Volume, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out *Volume
	err := s.withFileLock(func() error {
		if err := s.loadLocked(); err != nil {
			return err
		}
		if name == "" {
			return errors.New("volume name is required")
		}
		if v, ok := s.db.Volumes[name]; ok {
			out = cloneVolume(v)
			return nil
		}
		now := time.Now().UTC()
		v := &Volume{
			Name:       name,
			Mountpoint: mountpoint,
			Opts:       cloneMap(opts),
			ProjectID:  s.db.NextProjectID,
			Size:       firstNonEmpty(opts["size"], opts["quota"], opts["bhard"]),
			Inodes:     firstNonEmpty(opts["inodes"], opts["ihard"]),
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		s.db.NextProjectID++
		s.db.Volumes[name] = v
		if err := s.saveLocked(); err != nil {
			return err
		}
		out = cloneVolume(v)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) GetVolume(name string) (*Volume, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.db.Volumes[name]
	return cloneVolume(v), ok
}

func (s *Store) ListVolumes() []*Volume {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Volume, 0, len(s.db.Volumes))
	for _, v := range s.db.Volumes {
		out = append(out, cloneVolume(v))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Store) UpdateVolume(v *Volume) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.withFileLock(func() error {
		if err := s.loadLocked(); err != nil {
			return err
		}
		if _, ok := s.db.Volumes[v.Name]; !ok {
			return os.ErrNotExist
		}
		cp := cloneVolume(v)
		cp.UpdatedAt = time.Now().UTC()
		s.db.Volumes[v.Name] = cp
		return s.saveLocked()
	})
}

func (s *Store) DeleteVolume(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.withFileLock(func() error {
		if err := s.loadLocked(); err != nil {
			return err
		}
		if _, ok := s.db.Volumes[name]; !ok {
			return os.ErrNotExist
		}
		delete(s.db.Volumes, name)
		return s.saveLocked()
	})
}

func (s *Store) AddSnapshot(sn *Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.withFileLock(func() error {
		if err := s.loadLocked(); err != nil {
			return err
		}
		if sn.Name == "" || sn.Volume == "" {
			return errors.New("snapshot name and volume are required")
		}
		cp := *sn
		if cp.CreatedAt.IsZero() {
			cp.CreatedAt = time.Now().UTC()
		}
		s.db.Snapshots[cp.Name] = &cp
		return s.saveLocked()
	})
}

func (s *Store) GetSnapshot(name string) (*Snapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sn, ok := s.db.Snapshots[name]
	if !ok {
		return nil, false
	}
	cp := *sn
	return &cp, true
}

func (s *Store) ListSnapshots(volume string) []*Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []*Snapshot{}
	for _, sn := range s.db.Snapshots {
		if volume == "" || sn.Volume == volume {
			cp := *sn
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

func cloneVolume(v *Volume) *Volume {
	if v == nil {
		return nil
	}
	cp := *v
	cp.Opts = cloneMap(v.Opts)
	return &cp
}

func cloneMap(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func (s *Store) withFileLock(fn func() error) error {
	lockPath := s.path + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}
