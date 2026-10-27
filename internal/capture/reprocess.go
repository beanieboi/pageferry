package capture

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
)

// Reprocess renders the PDF in with pdftoppm at resolution_dpi and writes it
// to out the way a scan is stored: blank pages left out, pages without color
// in black and white, within max_pdf_bytes. It returns the number of pages
// kept.
func (c *Capturer) Reprocess(ctx context.Context, in, out string) (int, error) {
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	work, err := os.MkdirTemp(filepath.Dir(out), ".reprocess-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(work)
	if err := c.command(ctx, c.Config.PDFToPPMPath, "-r", strconv.Itoa(c.Config.ResolutionDPI), in, filepath.Join(work, "page")); err != nil {
		return 0, fmt.Errorf("render PDF: %w", err)
	}
	// pdftoppm pads the page numbers to the same width, so they sort.
	scans, err := filepath.Glob(filepath.Join(work, "page-*.p[pg]m"))
	if err != nil || len(scans) == 0 {
		return 0, errors.New("pdftoppm rendered no pages")
	}
	id := filepath.Base(in)
	pages, err := c.analyze(id, scans)
	if err != nil {
		return 0, err
	}
	pdfPath := filepath.Join(work, "reprocessed.pdf")
	if err := c.buildPDF(ctx, id, work, pages, pdfPath); err != nil {
		return 0, err
	}
	if err := os.Rename(pdfPath, out); err != nil {
		return 0, err
	}
	return len(pages), nil
}
