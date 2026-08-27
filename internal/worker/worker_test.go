package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/beanieboi/pageferry/internal/paperless"
	"github.com/beanieboi/pageferry/internal/spool"
)

type fakeAPI struct {
	uploadErr error
	task      paperless.Task
	uploads   int
}

func (f *fakeAPI) Upload(context.Context, string, string) (string, error) {
	f.uploads++
	return "task-1", f.uploadErr
}
func (f *fakeAPI) LookupTask(context.Context, string) (paperless.Task, bool, error) {
	return f.task, true, nil
}

func queuedJob(t *testing.T, store *spool.Store) spool.Job {
	t.Helper()
	id, work, err := store.BeginCapture(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("PDF")
	name := "scan.pdf"
	if err := os.WriteFile(filepath.Join(work, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	job := spool.Job{ID: id, Filename: name, CreatedAt: time.Now(), SizeBytes: int64(len(data)), SHA256: hex.EncodeToString(hash[:]), Pages: 1}
	if err := store.CommitCapture(work, job); err != nil {
		t.Fatal(err)
	}
	job, err = store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func TestSuccessDeletesPDFOnlyAfterTaskSuccess(t *testing.T) {
	store, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	job := queuedJob(t, store)
	api := &fakeAPI{task: paperless.Task{ID: "task-1", Status: "SUCCESS", DocumentID: 7}}
	w := &Worker{Store: store, API: api, PollInterval: time.Second, RetryInitial: time.Second, RetryMaximum: time.Minute}
	if err := w.ProcessOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.uploads != 1 {
		t.Fatalf("uploads=%d", api.uploads)
	}
	if _, err := os.Stat(store.DocumentPath(job)); !os.IsNotExist(err) {
		t.Fatalf("PDF still in queue: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.Completed, job.ID, "job.json")); err != nil {
		t.Fatal(err)
	}
}

func TestUploadFailureRetainsPDFAndSchedulesRetry(t *testing.T) {
	store, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	job := queuedJob(t, store)
	api := &fakeAPI{uploadErr: errors.New("offline")}
	now := time.Now()
	w := &Worker{Store: store, API: api, PollInterval: time.Second, RetryInitial: 30 * time.Second, RetryMaximum: time.Minute, Now: func() time.Time { return now }}
	if err := w.ProcessOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != spool.StateQueued || got.NextAttemptAt == nil || got.UploadAttempts != 1 {
		t.Fatalf("job=%#v", got)
	}
	if _, err := os.Stat(store.DocumentPath(got)); err != nil {
		t.Fatal("PDF was not retained")
	}
	if err := w.ProcessOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.uploads != 1 {
		t.Fatalf("retry happened too early: %d uploads", api.uploads)
	}
}
