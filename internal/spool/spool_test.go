package spool

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCommitSurvivesReopenAndFinishRemovesPDF(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id, work, err := store.BeginCapture(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "scan.pdf"), []byte("pdf"), 0o600); err != nil {
		t.Fatal(err)
	}
	job := Job{ID: id, Filename: "scan.pdf", CreatedAt: time.Now(), SizeBytes: 3, SHA256: "abc", Pages: 1}
	if err := store.CommitCapture(work, job); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(store.Root)
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := reopened.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].State != StateQueued {
		t.Fatalf("jobs: %#v", jobs)
	}
	if err := reopened.Finish(jobs[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(reopened.Completed, id, "scan.pdf")); !os.IsNotExist(err) {
		t.Fatalf("PDF should have been removed, stat error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(reopened.Completed, id, "job.json")); err != nil {
		t.Fatalf("receipt missing: %v", err)
	}
}

func TestLockIsExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	first, err := TryLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := TryLock(path); err == nil {
		second.Close()
		t.Fatal("second lock unexpectedly succeeded")
	}
}
