package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// record is one attempt at an archive: unzipped ("ok") or "failed". An
// archive is done once it has a record for its current ETag; "preprocessor
// retry" removes failed records so they are tried again.
type record struct {
	Job     string    `json:"job"`
	Bucket  string    `json:"bucket"`
	Key     string    `json:"key"`
	ETag    string    `json:"etag"`
	Size    int64     `json:"size"`
	Status  string    `json:"status"`
	Entries int       `json:"entries"` // files in the archive
	Written int       `json:"written"` // written to the bucket
	Skipped int       `json:"skipped"` // already there, unchanged
	Error   string    `json:"error,omitempty"`
	At      time.Time `json:"at"`
}

func (r record) id() string { return r.Job + "\x00" + r.Bucket + "\x00" + r.Key + "\x00" + r.ETag }

// store keeps the records in one JSON file. The service and preprocessorctl
// (status, retry) both change it, so every access holds a lock on file.lock.
type store struct{ file string }

func (s *store) lock(how int) (func(), error) {
	f, err := os.OpenFile(s.file+".lock", os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

func (s *store) read() ([]record, error) {
	b, err := os.ReadFile(s.file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rs []record
	if err := json.Unmarshal(b, &rs); err != nil {
		return nil, err
	}
	return rs, nil
}

func (s *store) load() ([]record, error) {
	unlock, err := s.lock(syscall.LOCK_SH)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return s.read()
}

// update replaces the records with fn's result, atomically.
func (s *store) update(fn func([]record) []record) error {
	unlock, err := s.lock(syscall.LOCK_EX)
	if err != nil {
		return err
	}
	defer unlock()
	rs, err := s.read()
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(fn(rs), "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.file), ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.file)
}

func (s *store) add(r record) error {
	return s.update(func(rs []record) []record { return append(rs, r) })
}
