package capture

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beanieboi/pageferry/internal/config"
	"github.com/beanieboi/pageferry/internal/spool"
)

func TestParsePageCount(t *testing.T) {
	pages, err := parsePageCount("Title: x\nPages:          12\nEncrypted: no\n")
	if err != nil {
		t.Fatal(err)
	}
	if pages != 12 {
		t.Fatalf("got %d", pages)
	}
}

// TestRun scans a page with text and a red logo and a blank page through a
// fake scanimage, with the real libtiff and poppler tools.
func TestRun(t *testing.T) {
	c := config.Default()
	for _, tool := range []struct {
		name string
		path *string
	}{{"tiffcp", &c.TIFFCPPath}, {"tiff2pdf", &c.TIFF2PDFPath}, {"pdfinfo", &c.PDFInfoPath}} {
		path, err := exec.LookPath(tool.name)
		if err != nil {
			t.Skipf("%s is not installed", tool.name)
		}
		*tool.path = path
	}
	dir := t.TempDir()
	pages := filepath.Join(dir, "pages")
	if err := os.Mkdir(pages, 0o700); err != nil {
		t.Fatal(err)
	}
	writePage(t, filepath.Join(pages, "1.pnm"), true)
	writePage(t, filepath.Join(pages, "2.pnm"), false)
	c.ScanimagePath = filepath.Join(dir, "scanimage")
	script := `#!/bin/sh
for arg; do case "$arg" in --batch=*) pattern=${arg#--batch=};; esac; done
n=1
for page in ` + pages + `/*.pnm; do cp "$page" "$(printf "$pattern" $n)"; n=$((n+1)); done
`
	if err := os.WriteFile(c.ScanimagePath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	c.SpoolDir = filepath.Join(dir, "spool")
	c.MinFreeBytes = 0
	// Unreachable, so every way of shrinking the color page is tried.
	c.MaxPDFBytes = 1
	store, err := spool.Open(c.SpoolDir)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	job, err := (&Capturer{Config: c, Store: store, Logger: slog.New(slog.NewTextHandler(&logs, nil))}).Run(context.Background())
	if err != nil {
		t.Fatalf("%v\n%s", err, logs.String())
	}
	if job.Pages != 1 {
		t.Fatalf("got %d pages, want the blank one skipped", job.Pages)
	}
	if !strings.Contains(logs.String(), "class=color") || !strings.Contains(logs.String(), "dpi=150 quality=50") {
		t.Fatalf("color page not shrunk:\n%s", logs.String())
	}
	entries, err := os.ReadDir(store.JobPath(job.ID))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if want := []string{"job.json", job.Filename}; fmt.Sprint(names) != fmt.Sprint(want) {
		t.Fatalf("job contains %v, want %v", names, want)
	}
}

// writePage writes an A4 page at 300 DPI, blank or with lines of text and a
// red logo.
func writePage(t *testing.T, path string, content bool) {
	const width, height = 2480, 3508
	pix := bytes.Repeat([]byte{250}, width*height*3)
	fill := func(x0, y0, x1, y1 int, rgb ...byte) {
		for y := y0; y < y1; y++ {
			for x := x0; x < x1; x++ {
				copy(pix[(y*width+x)*3:], rgb)
			}
		}
	}
	if content {
		for y := 500; y < 1500; y += 60 {
			for x := 300; x < 2100; x += 6 {
				fill(x, y, x+3, y+30, 20, 20, 20)
			}
		}
		fill(1900, 250, 2018, 368, 230, 0, 0)
	}
	data := append([]byte(fmt.Sprintf("P6\n%d %d\n255\n", width, height)), pix...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
