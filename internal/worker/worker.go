package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/beanieboi/pageferry/internal/paperless"
	"github.com/beanieboi/pageferry/internal/spool"
)

type API interface {
	Upload(context.Context, string, string) (string, error)
	LookupTask(context.Context, string) (paperless.Task, bool, error)
}

type Worker struct {
	Store        *spool.Store
	API          API
	Logger       *slog.Logger
	PollInterval time.Duration
	RetryInitial time.Duration
	RetryMaximum time.Duration
	Now          func() time.Time
}

func (w *Worker) Run(ctx context.Context) error {
	if w.Logger == nil {
		w.Logger = slog.Default()
	}
	lock, err := spool.TryLock(w.Store.Root + "/uploader.lock")
	if err != nil {
		return fmt.Errorf("another uploader is already running: %w", err)
	}
	defer lock.Close()
	for {
		if err := w.ProcessOnce(ctx); err != nil {
			w.Logger.Error("queue pass failed", "error", err)
		}
		timer := time.NewTimer(w.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func (w *Worker) ProcessOnce(ctx context.Context) error {
	if w.Now == nil {
		w.Now = time.Now
	}
	if w.Logger == nil {
		w.Logger = slog.Default()
	}
	jobs, err := w.Store.List()
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch job.State {
		case spool.StateCompleted:
			if err := w.Store.Finish(job); err != nil {
				return err
			}
		case spool.StateSubmitted:
			if err := w.poll(ctx, job); err != nil {
				w.Logger.Warn("Paperless task poll failed", "job_id", job.ID, "error", err)
			}
		case spool.StateQueued, spool.StateUploading:
			if job.NextAttemptAt != nil && w.Now().Before(*job.NextAttemptAt) {
				continue
			}
			if err := w.upload(ctx, job); err != nil {
				w.Logger.Warn("Paperless upload deferred", "job_id", job.ID, "error", err)
			}
		case spool.StateFailed:
			continue
		default:
			return fmt.Errorf("job %s has unknown state %q", job.ID, job.State)
		}
	}
	return nil
}

func (w *Worker) upload(ctx context.Context, job spool.Job) error {
	path := w.Store.DocumentPath(job)
	if err := verify(path, job.SizeBytes, job.SHA256); err != nil {
		job.State = spool.StateFailed
		job.LastError = "queued PDF failed integrity check: " + err.Error()
		if saveErr := w.Store.Save(job); saveErr != nil {
			return saveErr
		}
		return errorsWithContext(job.LastError)
	}
	job.State = spool.StateUploading
	job.UploadAttempts++
	job.LastError = ""
	if err := w.Store.Save(job); err != nil {
		return err
	}
	taskID, err := w.API.Upload(ctx, path, job.Filename)
	if err != nil {
		job.State = spool.StateQueued
		job.LastError = err.Error()
		next := w.Now().Add(w.backoff(job.UploadAttempts))
		job.NextAttemptAt = &next
		if saveErr := w.Store.Save(job); saveErr != nil {
			return fmt.Errorf("%v; also failed to save retry: %w", err, saveErr)
		}
		return err
	}
	job.State = spool.StateSubmitted
	job.TaskID = taskID
	job.NextAttemptAt = nil
	job.LastError = ""
	if err := w.Store.Save(job); err != nil {
		return fmt.Errorf("persist Paperless task id %s: %w", taskID, err)
	}
	w.Logger.Info("PDF accepted by Paperless", "job_id", job.ID, "task_id", taskID, "attempt", job.UploadAttempts)
	return w.poll(ctx, job)
}

func (w *Worker) poll(ctx context.Context, job spool.Job) error {
	task, found, err := w.API.LookupTask(ctx, job.TaskID)
	if err != nil {
		job.LastError = err.Error()
		_ = w.Store.Save(job)
		return err
	}
	if !found {
		return nil
	}
	switch strings.ToUpper(task.Status) {
	case "SUCCESS":
		job.DocumentID = task.DocumentID
		if err := w.Store.Finish(job); err != nil {
			return err
		}
		w.Logger.Info("Paperless consumption succeeded", "job_id", job.ID, "task_id", job.TaskID, "document_id", task.DocumentID)
	case "FAILURE", "REVOKED":
		job.State = spool.StateFailed
		job.LastError = strings.TrimSpace(task.Result)
		if job.LastError == "" {
			job.LastError = "Paperless task ended with status " + task.Status
		}
		if err := w.Store.Save(job); err != nil {
			return err
		}
		w.Logger.Error("Paperless consumption failed; PDF retained", "job_id", job.ID, "task_id", job.TaskID, "status", task.Status, "error", job.LastError)
	default:
		if job.LastError != "" {
			job.LastError = ""
			return w.Store.Save(job)
		}
	}
	return nil
}

func (w *Worker) backoff(attempt int) time.Duration {
	delay := w.RetryInitial
	for i := 1; i < attempt && delay < w.RetryMaximum; i++ {
		if delay > w.RetryMaximum/2 {
			return w.RetryMaximum
		}
		delay *= 2
	}
	if delay > w.RetryMaximum {
		return w.RetryMaximum
	}
	return delay
}

func verify(path string, expectedSize int64, expectedHash string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return err
	}
	if stat.Size() != expectedSize {
		return fmt.Errorf("size is %d, expected %d", stat.Size(), expectedSize)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	actual := hex.EncodeToString(h.Sum(nil))
	if actual != expectedHash {
		return fmt.Errorf("SHA-256 is %s, expected %s", actual, expectedHash)
	}
	return nil
}

func errorsWithContext(message string) error { return fmt.Errorf("%s", message) }
