package capture

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/beanieboi/pageferry/internal/config"
	"github.com/beanieboi/pageferry/internal/imaging"
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
		pages, _ := filepath.Glob(filepath.Join(work, "page-*.pnm"))
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

	pagePattern := filepath.Join(work, "page-%04d.pnm")
	device := c.Config.Device
	if scanbdDevice := os.Getenv("SCANBD_DEVICE"); scanbdDevice != "" {
		device = scanbdDevice
	}
	args := []string{"--batch=" + pagePattern, "--format=pnm", "--source", c.Config.Source,
		"--mode", c.Config.Mode, "--resolution", strconv.Itoa(c.Config.ResolutionDPI),
		"--page-width", strconv.Itoa(c.Config.PageWidthMM), "--page-height", strconv.Itoa(c.Config.PageHeightMM)}
	args = append(args, c.Config.ScanOptions...)
	if device != "" {
		args = append([]string{"--device-name", device}, args...)
	}
	if err := c.command(ctx, c.Config.ScanimagePath, args...); err != nil {
		return job, fmt.Errorf("scan pages: %w", err)
	}
	scans, err := filepath.Glob(filepath.Join(work, "page-*.pnm"))
	if err != nil || len(scans) == 0 {
		return job, errors.New("scanner returned no pages")
	}
	pages, err := c.analyze(id, scans)
	if err != nil {
		return job, err
	}

	filename := fmt.Sprintf("scan-%s-%s.pdf", now.Format("2006-01-02_15-04-05"), shortID(id))
	pdfPath := filepath.Join(work, filename)
	if err := c.buildPDF(ctx, id, work, pages, pdfPath); err != nil {
		return job, err
	}
	pageCount := len(pages)

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

	for _, path := range scans {
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

type page struct {
	scan     string
	analysis imaging.Analysis
}

// analyze classifies the scanned pages and leaves out blank ones, unless
// all of them are blank.
func (c *Capturer) analyze(id string, scans []string) ([]page, error) {
	thresholds := imaging.Thresholds{
		BlankContentMM2: c.Config.BlankPageThresholdMM2,
		ColorMM2:        c.Config.ColorThresholdMM2,
		GrayMM2:         c.Config.GrayThresholdMM2,
	}
	var pages, blank []page
	for i, scan := range scans {
		img, err := imaging.ReadPNMFile(scan)
		if err != nil {
			return nil, fmt.Errorf("read scanned page: %w", err)
		}
		a := imaging.Analyze(img, c.Config.ResolutionDPI, thresholds)
		c.Logger.Info("page analyzed", "job_id", id, "page", i+1, "class", a.Class, "blank", a.Blank,
			"content_mm2", math.Round(a.ContentMM2), "color_mm2", math.Round(a.ColorMM2), "gray_mm2", math.Round(a.GrayMM2),
			"paper", a.Paper, "threshold", a.Threshold)
		p := page{scan: scan, analysis: a}
		if a.Blank && c.Config.SkipBlankPages {
			blank = append(blank, p)
			continue
		}
		pages = append(pages, p)
	}
	if len(pages) == 0 {
		c.Logger.Warn("every page looks blank; keeping them all", "job_id", id, "pages", len(blank))
		return blank, nil
	}
	if len(blank) > 0 {
		c.Logger.Info("blank pages skipped", "job_id", id, "skipped", len(blank), "kept", len(pages))
	}
	return pages, nil
}

// buildPDF writes the pages to pdfPath: black and white pages at the scan
// resolution (CCITT G4), color and grayscale pages as JPEG at
// color_resolution_dpi. While the PDF exceeds max_pdf_bytes, the color and
// grayscale pages are made smaller: first their resolution, down to
// minColorDPI, then their JPEG quality, down to minPDFQuality. Below that,
// Paperless's OCR suffers more than its viewer benefits.
func (c *Capturer) buildPDF(ctx context.Context, id, work string, pages []page, pdfPath string) error {
	images := 0
	for _, p := range pages {
		if p.analysis.Class != imaging.BW {
			images++
		}
	}
	dpi, quality := c.Config.ColorResolutionDPI, c.Config.PDFQuality
	limit := c.Config.MaxPDFBytes
	for {
		size, err := c.writePDF(ctx, work, pages, dpi, quality, pdfPath)
		if err != nil {
			return err
		}
		if limit == 0 || size <= limit || images == 0 {
			return nil
		}
		switch {
		case dpi > minColorDPI:
			dpi = max(minColorDPI, dpi*3/4)
		case quality > minPDFQuality:
			quality = max(minPDFQuality, quality-10)
		default:
			c.Logger.Warn("PDF is larger than max_pdf_bytes", "job_id", id, "bytes", size, "max_pdf_bytes", limit)
			return nil
		}
		c.Logger.Info("PDF too large; shrinking color pages", "job_id", id, "bytes", size, "max_pdf_bytes", limit, "dpi", dpi, "quality", quality)
	}
}

const minColorDPI, minPDFQuality = 150, 50

func (c *Capturer) writePDF(ctx context.Context, work string, pages []page, colorDPI, quality int, pdfPath string) (size int64, returnErr error) {
	combined := filepath.Join(work, "combined.tif")
	var tiffs []string
	defer func() {
		for _, path := range append(tiffs, combined) {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) && returnErr == nil {
				returnErr = fmt.Errorf("remove intermediate %s: %w", path, err)
			}
		}
	}()
	for i, p := range pages {
		img, err := imaging.ReadPNMFile(p.scan)
		if err != nil {
			return 0, fmt.Errorf("read scanned page: %w", err)
		}
		path := filepath.Join(work, fmt.Sprintf("out-%04d.tif", i+1))
		dpi := float64(c.Config.ResolutionDPI)
		switch p.analysis.Class {
		case imaging.BW:
			err = img.Threshold(p.analysis.Threshold).WriteTIFFFile(path, dpi)
		default:
			if p.analysis.Class == imaging.Gray {
				img = img.Gray()
			}
			img.Whiten(p.analysis.Paper)
			width := int(math.Round(float64(img.Width) * float64(colorDPI) / dpi))
			height := int(math.Round(float64(img.Height) * float64(colorDPI) / dpi))
			scaled := img.Downscale(width, height)
			err = scaled.WriteTIFFFile(path, dpi*float64(scaled.Width)/float64(img.Width))
		}
		tiffs = append(tiffs, path)
		if err != nil {
			return 0, fmt.Errorf("write page %d: %w", i+1, err)
		}
	}

	if err := c.command(ctx, c.Config.TIFFCPPath, append(tiffs, combined)...); err != nil {
		return 0, fmt.Errorf("combine TIFF pages: %w", err)
	}
	if err := c.command(ctx, c.Config.TIFF2PDFPath, "-j", "-q", strconv.Itoa(quality), "-o", pdfPath, combined); err != nil {
		return 0, fmt.Errorf("create PDF: %w", err)
	}
	info, err := c.commandOutput(ctx, c.Config.PDFInfoPath, pdfPath)
	if err != nil {
		return 0, fmt.Errorf("validate PDF: %w", err)
	}
	pageCount, err := parsePageCount(info)
	if err != nil {
		return 0, err
	}
	if pageCount != len(pages) {
		return 0, fmt.Errorf("PDF contains %d pages but %d were kept", pageCount, len(pages))
	}
	stat, err := os.Stat(pdfPath)
	if err != nil {
		return 0, err
	}
	return stat.Size(), nil
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
