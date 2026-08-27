package spool

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	StateQueued    = "queued"
	StateUploading = "uploading"
	StateSubmitted = "submitted"
	StateFailed    = "failed"
	StateCompleted = "completed"
)

type Job struct {
	Version        int        `json:"version"`
	ID             string     `json:"id"`
	Filename       string     `json:"filename"`
	State          string     `json:"state"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
	SizeBytes      int64      `json:"size_bytes"`
	SHA256         string     `json:"sha256"`
	Pages          int        `json:"pages"`
	UploadAttempts int        `json:"upload_attempts"`
	TaskID         string     `json:"paperless_task_id,omitempty"`
	DocumentID     int        `json:"paperless_document_id,omitempty"`
	LastError      string     `json:"last_error,omitempty"`
	NextAttemptAt  *time.Time `json:"next_attempt_at,omitempty"`
}

type Store struct {
	Root      string
	Jobs      string
	Work      string
	Failed    string
	Completed string
}

func Open(root string) (*Store, error) {
	s := &Store{
		Root: root, Jobs: filepath.Join(root, "jobs"), Work: filepath.Join(root, "work"),
		Failed: filepath.Join(root, "failed-captures"), Completed: filepath.Join(root, "completed"),
	}
	for _, dir := range []string{s.Root, s.Jobs, s.Work, s.Failed, s.Completed} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create spool directory %s: %w", dir, err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return nil, fmt.Errorf("secure spool directory %s: %w", dir, err)
		}
	}
	return s, nil
}

func NewID(now time.Time) (string, error) {
	random := make([]byte, 6)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return now.UTC().Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(random), nil
}

func (s *Store) BeginCapture(now time.Time) (id, path string, err error) {
	id, err = NewID(now)
	if err != nil {
		return "", "", fmt.Errorf("create job id: %w", err)
	}
	path = filepath.Join(s.Work, ".capture-"+id)
	if err := os.Mkdir(path, 0o700); err != nil {
		return "", "", fmt.Errorf("create capture workspace: %w", err)
	}
	return id, path, nil
}

func (s *Store) CommitCapture(workPath string, job Job) error {
	if filepath.Dir(workPath) != s.Work || !strings.HasPrefix(filepath.Base(workPath), ".capture-") {
		return errors.New("refusing to commit an invalid capture workspace")
	}
	job.Version = 1
	job.State = StateQueued
	job.UpdatedAt = time.Now().UTC()
	if job.CreatedAt.IsZero() {
		job.CreatedAt = job.UpdatedAt
	}
	if err := writeJSONAtomic(workPath, job); err != nil {
		return err
	}
	if err := syncDir(workPath); err != nil {
		return err
	}
	destination := filepath.Join(s.Jobs, job.ID)
	if err := os.Rename(workPath, destination); err != nil {
		return fmt.Errorf("publish queued job: %w", err)
	}
	return syncDir(s.Jobs)
}

func (s *Store) PreserveFailedCapture(workPath, id string) error {
	if _, err := os.Stat(workPath); err != nil {
		return err
	}
	destination := filepath.Join(s.Failed, id)
	if err := os.Rename(workPath, destination); err != nil {
		return fmt.Errorf("preserve failed capture: %w", err)
	}
	return syncDir(s.Failed)
}

func (s *Store) JobPath(id string) string { return filepath.Join(s.Jobs, filepath.Base(id)) }

func (s *Store) DocumentPath(job Job) string {
	return filepath.Join(s.JobPath(job.ID), job.Filename)
}

func (s *Store) Load(id string) (Job, error) {
	return loadJSON(filepath.Join(s.JobPath(id), "job.json"))
}

func (s *Store) Save(job Job) error {
	job.UpdatedAt = time.Now().UTC()
	return writeJSONAtomic(s.JobPath(job.ID), job)
}

func (s *Store) List() ([]Job, error) {
	entries, err := os.ReadDir(s.Jobs)
	if err != nil {
		return nil, err
	}
	jobs := make([]Job, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		job, err := s.Load(entry.Name())
		if err != nil {
			return nil, fmt.Errorf("load job %s: %w", entry.Name(), err)
		}
		jobs = append(jobs, job)
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].CreatedAt.Before(jobs[j].CreatedAt) })
	return jobs, nil
}

func (s *Store) Finish(job Job) error {
	now := time.Now().UTC()
	job.State = StateCompleted
	job.CompletedAt = &now
	job.NextAttemptAt = nil
	job.LastError = ""
	if err := s.Save(job); err != nil {
		return err
	}
	source := s.JobPath(job.ID)
	destination := filepath.Join(s.Completed, job.ID)
	if err := os.Rename(source, destination); err != nil {
		return fmt.Errorf("archive completed job receipt: %w", err)
	}
	if err := syncDir(s.Jobs); err != nil {
		return err
	}
	if err := syncDir(s.Completed); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(destination, job.Filename)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove delivered PDF: %w", err)
	}
	return syncDir(destination)
}

func (s *Store) Retry(id string) error {
	job, err := s.Load(id)
	if err != nil {
		return err
	}
	if job.State != StateFailed {
		return fmt.Errorf("job %s is %s, not failed", id, job.State)
	}
	job.State = StateQueued
	job.TaskID = ""
	job.LastError = ""
	job.NextAttemptAt = nil
	return s.Save(job)
}

func (s *Store) PruneCompleted(olderThan time.Duration) error {
	if olderThan == 0 {
		return nil
	}
	entries, err := os.ReadDir(s.Completed)
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-olderThan)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.ModTime().Before(cutoff) {
			if err := os.RemoveAll(filepath.Join(s.Completed, entry.Name())); err != nil {
				return err
			}
		}
	}
	return syncDir(s.Completed)
}

type Lock struct{ file *os.File }

func TryLock(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return &Lock{file: f}, nil
}

func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	return l.file.Close()
}

func writeJSONAtomic(dir string, job Job) error {
	data, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := filepath.Join(dir, ".job.json.tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write job manifest: %w", err)
	}
	f, err := os.OpenFile(tmp, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, "job.json")); err != nil {
		return fmt.Errorf("publish job manifest: %w", err)
	}
	return syncDir(dir)
}

func loadJSON(path string) (Job, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Job{}, err
	}
	var job Job
	if err := json.Unmarshal(data, &job); err != nil {
		return Job{}, err
	}
	if job.ID == "" || job.Filename == "" || job.State == "" {
		return Job{}, errors.New("incomplete job manifest")
	}
	return job, nil
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
