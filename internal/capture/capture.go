package capture

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/beanieboi/pageferry/internal/config"
	"github.com/beanieboi/pageferry/internal/spool"
)

type Capturer struct {
	Config config.Config
	Store  *spool.Store
	Logger *slog.Logger
	Now    func() time.Time
}

func (c *Capturer) Run(ctx context.Context) (job spool.Job, returnErr error) {
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	lock, err := spool.TryLock(filepath.Join(c.Store.Root, "capture.lock"))
	if err != nil {
		return job, fmt.Errorf("another capture is already running: %w", err)
	}
	defer lock.Close()

	if err := requireFreeSpace(c.Store.Root, c.Config.MinFreeBytes); err != nil {
		return job, err
	}
	now := c.Now()
	id, work, err := c.Store.BeginCapture(now)
	if err != nil {
		return job, err
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		pages, _ := filepath.Glob(filepath.Join(work, "page-*.tif"))
		if len(pages) == 0 {
			_ = os.RemoveAll(work)
			return
		}
		if err := c.Store.PreserveFailedCapture(work, id); err != nil {
			c.Logger.Error("could not preserve failed capture", "job_id", id, "error", err)
		} else {
			c.Logger.Error("capture failed; raw pages retained", "job_id", id, "path", filepath.Join(c.Store.Failed, id))
		}
	}()

	pagePattern := filepath.Join(work, "page-%04d.tif")
	device := c.Config.Device
	if scanbdDevice := os.Getenv("SCANBD_DEVICE"); scanbdDevice != "" {
		device = scanbdDevice
	}
	args := []string{"--batch=" + pagePattern, "--format=tiff", "--source", c.Config.Source,
		"--mode", c.Config.Mode, "--resolution", strconv.Itoa(c.Config.ResolutionDPI),
		"--page-width", strconv.Itoa(c.Config.PageWidthMM), "--page-height", strconv.Itoa(c.Config.PageHeightMM)}
	args = append(args, c.Config.ScanOptions...)
	if device != "" {
		args = append([]string{"--device-name", device}, args...)
	}
	if err := c.command(ctx, c.Config.ScanimagePath, args...); err != nil {
		return job, fmt.Errorf("scan pages: %w", err)
	}
	pages, err := filepath.Glob(filepath.Join(work, "page-*.tif"))
	if err != nil || len(pages) == 0 {
		return job, errors.New("scanner returned no pages")
	}

	combined := filepath.Join(work, "combined.tif")
	tiffArgs := append([]string{"-c", "zip"}, pages...)
	tiffArgs = append(tiffArgs, combined)
	if err := c.command(ctx, c.Config.TIFFCPPath, tiffArgs...); err != nil {
		return job, fmt.Errorf("combine TIFF pages: %w", err)
	}
	filename := fmt.Sprintf("scan-%s-%s.pdf", now.Format("2006-01-02_15-04-05"), shortID(id))
	pdfPath := filepath.Join(work, filename)
	if err := c.command(ctx, c.Config.TIFF2PDFPath, "-j", "-q", strconv.Itoa(c.Config.PDFQuality), "-o", pdfPath, combined); err != nil {
		return job, fmt.Errorf("create PDF: %w", err)
	}
	info, err := c.commandOutput(ctx, c.Config.PDFInfoPath, pdfPath)
	if err != nil {
		return job, fmt.Errorf("validate PDF: %w", err)
	}
	pageCount, err := parsePageCount(info)
	if err != nil {
		return job, err
	}
	if pageCount != len(pages) {
		return job, fmt.Errorf("PDF contains %d pages but scanner produced %d", pageCount, len(pages))
	}

	file, err := os.OpenFile(pdfPath, os.O_RDWR, 0)
	if err != nil {
		return job, err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return job, err
	}
	if err := file.Close(); err != nil {
		return job, err
	}
	stat, err := os.Stat(pdfPath)
	if err != nil {
		return job, err
	}
	digest, err := hashFile(pdfPath)
	if err != nil {
		return job, err
	}

	for _, path := range append(pages, combined) {
		if err := os.Remove(path); err != nil {
			return job, fmt.Errorf("remove intermediate %s: %w", path, err)
		}
	}
	job = spool.Job{ID: id, Filename: filename, CreatedAt: now.UTC(), SizeBytes: stat.Size(), SHA256: digest, Pages: pageCount}
	if err := c.Store.CommitCapture(work, job); err != nil {
		return job, err
	}
	committed = true
	c.Logger.Info("scan queued durably", "job_id", id, "filename", filename, "pages", pageCount, "bytes", stat.Size())
	return job, nil
}

func (c *Capturer) command(ctx context.Context, path string, args ...string) error {
	_, err := c.commandOutput(ctx, path, args...)
	return err
}

func (c *Capturer) commandOutput(ctx context.Context, path string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = append(os.Environ(), "SANE_CONFIG_DIR="+c.Config.SANEConfigDir)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	c.Logger.Debug("running command", "path", path, "args", args)
	if err := cmd.Run(); err != nil {
		return output.String(), fmt.Errorf("%s: %w: %s", filepath.Base(path), err, strings.TrimSpace(output.String()))
	}
	return output.String(), nil
}

func parsePageCount(info string) (int, error) {
	for _, line := range strings.Split(info, "\n") {
		key, value, found := strings.Cut(line, ":")
		if found && strings.TrimSpace(key) == "Pages" {
			pages, err := strconv.Atoi(strings.TrimSpace(value))
			if err == nil && pages > 0 {
				return pages, nil
			}
		}
	}
	return 0, errors.New("pdfinfo did not report a positive page count")
}

func requireFreeSpace(path string, minimum uint64) error {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return fmt.Errorf("check free spool space: %w", err)
	}
	available := stat.Bavail * uint64(stat.Bsize)
	if available < minimum {
		return fmt.Errorf("not enough free spool space: %d bytes available, %d required", available, minimum)
	}
	return nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func shortID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[len(id)-12:]
}
