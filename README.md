# PageFerry

PageFerry turns the ScanSnap button into a reliable, image-only PDF handoff to Paperless-ngx. It is one Go binary with two roles:

- `pageferry-button` (a symlink to the binary) is invoked by `scanbd`. It drains the ADF, drops blank pages, reduces pages without color to black and white, builds and validates one compact PDF, and atomically publishes it to a durable local queue.
- `pageferry run` uploads queued PDFs as a streaming multipart request, persists Paperless's task UUID, and polls the task until it reaches `SUCCESS`.

The PDF is deleted only after Paperless reports successful consumption. Network errors, restarts, and Paperless downtime leave the PDF in the queue. Failed Paperless tasks retain the PDF for inspection and manual retry.

## Why the queue is on disk

Keeping the scan only in memory would lose it on a process crash, power loss, reboot, broken connection, or Paperless outage. The durable spool is the reliability boundary: files and metadata are fsynced and published with an atomic rename before the scanner action returns. Uploading is streamed from that file, so memory use stays nearly constant regardless of document size.

There is one unavoidable distributed-systems edge case: if Paperless accepts an upload but the HTTP response containing its task UUID is lost, a later retry may submit the same content again. The unique filename and Paperless's duplicate detection reduce the practical impact, but the Paperless upload API does not currently expose an idempotency key.

## Build and test

The repository uses `mise`:

```sh
mise install
mise exec -- go test ./...
mise exec -- go build -o pageferry .
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 mise exec -- go build -trimpath -o pageferry-linux-arm64 .
```

The binary uses only the Go standard library. Scanner acquisition and PDF construction deliberately use the mature system tools already required by `scanbd`: `scanimage`, `tiffcp`, `tiff2pdf`, and `pdfinfo`. No shell is involved. Page analysis and image preparation (`internal/imaging`) are plain Go.

The tests that run the whole capture pipeline need `tiffcp`, `tiff2pdf`, and `pdfinfo` on the `PATH` and are skipped otherwise:

```sh
nix shell nixpkgs#libtiff nixpkgs#poppler-utils -c go test ./...
```

## Installation

Install with Go, then copy and edit the example configuration:

```sh
go install github.com/beanieboi/pageferry@latest
sudo install -Dm640 config.example.json /etc/pageferry/config.json
```

## Configuration

Copy `config.example.json` to `/etc/pageferry/config.json`. Put `PAPERLESS_URL` and `PAPERLESS_TOKEN` in `/etc/pageferry/paperless.env`, owned by `root:scanner`, mode `0640`. The file is parsed directly as data—never sourced by a shell—and the token is never accepted as a command-line argument or logged.

Important scanner settings:

- `resolution_dpi`: scan resolution, normally 300.
- `source`: `ADF Duplex` scans both sides; use the scanner-supported front-only source for simplex.
- `mode`: `Color` (recommended), `Gray`, or `Lineart`, depending on the SANE backend. PageFerry decides per page how to store it, so scan in color and let it reduce pages; `Lineart` leaves no choice but black and white.
- `pdf_quality`: JPEG quality of color and grayscale pages.
- `scan_options`: backend-specific SANE options. The supplied iX1300 example preserves the settings validated on this scanner. Leave the backend's own blank page skipping (`--swskip`) off; PageFerry's is more reliable.
- `color_resolution_dpi`: resolution of color and grayscale pages in the PDF (default 200). Black and white pages keep `resolution_dpi`.
- `max_pdf_bytes`: size the PDF should stay below (default 1 MiB, 0 disables it), see below.
- `skip_blank_pages`: leave out blank pages (default `true`).
- `blank_page_threshold_mm2`, `color_threshold_mm2`, `gray_threshold_mm2`: detection thresholds, see below.

One button press drains every sheet currently in the ADF into one multi-page PDF. PageFerry does no OCR; Paperless is responsible for OCR and archival processing.

## Page processing

Each scanned page is analyzed in cells of about 0.34 mm, leaving out an 8 mm margin at the top and bottom and 16 mm at the sides, where the paper edge, its shadow, and punch holes are.

- **Blank pages**: a page with less than `blank_page_threshold_mm2` (100 mm²) of content is left out. Content is connected ink that is neither dust (smaller than 2 mm²) nor a line thinner than 1 mm, such as a fold crease. Bleed-through from the other side is lighter than ink and does not count; pencil and faded receipts do. A page with color (see below) is never blank, so faint colored writing is kept. If every page of a scan is blank, all are kept.
- **Color**: a page with more than `color_threshold_mm2` (15 mm², a 4 x 4 mm logo) of clearly colored area stays in color. Colored stamps, logos, and pen marks count; the color fringes scanners leave along black text, and lightly tinted paper, do not.
- **Grayscale**: a page without color but with more than `gray_threshold_mm2` (400 mm²) of evenly gray area, such as a photo or shading, is stored in grayscale.
- **Black and white**: every other page is thresholded at the scan resolution and stored with CCITT G4, typically 20 to 80 KB per page.

Color and grayscale pages are reduced to `color_resolution_dpi`, the paper's texture is whitened, and they are stored as JPEG, about 150 to 400 KB per A4 page at 200 DPI. While the PDF is larger than `max_pdf_bytes`, PageFerry lowers their resolution to 150 DPI and then their JPEG quality to 50; below that Paperless's OCR would suffer. A PDF that is still too large is kept and logged.

`pageferry reprocess IN.pdf OUT.pdf` treats an existing image-only PDF the same way, after rendering it at `resolution_dpi` with `pdftoppm` (`pdftoppm_path`). It is meant for scans that predate this processing; a PDF with real text or vector graphics would only lose quality.

Every page's measurements are logged (`page analyzed`, with `content_mm2`, `color_mm2`, and `gray_mm2`), so the thresholds can be tuned from the journal.

## Install on the Pi

```sh
install -o root -g root -m 0755 pageferry-linux-arm64 /usr/local/sbin/pageferry
ln -sfn /usr/local/sbin/pageferry /usr/local/sbin/pageferry-button
ln -sfn /usr/local/sbin/pageferry /etc/scanbd/scripts/pageferry-button
install -d -o root -g scanner -m 0750 /etc/pageferry
install -o root -g scanner -m 0640 config.json /etc/pageferry/config.json
install -o root -g scanner -m 0640 .env /etc/pageferry/paperless.env
install -o root -g root -m 0644 pageferry.service /etc/systemd/system/pageferry.service
systemctl daemon-reload
systemctl enable --now pageferry.service
```

Set the scan action in `/etc/scanbd/scanner.d/fujitsu.conf` to:

```text
script = "pageferry-button"
```

`scanbd` always resolves action names below its configured `scriptdir`, so the second symlink is deliberate. It executes the Go binary directly; it is not a shell wrapper. Then restart `scanbd`.

## Operations

```sh
pageferry status
pageferry check
journalctl -u pageferry.service -f
pageferry retry JOB_ID
```

Queue layout under `/var/lib/pageferry`:

- `jobs/`: PDFs awaiting upload, Paperless completion, or manual retry.
- `failed-captures/`: raw scanned pages (PNM) retained when PDF construction fails.
- `completed/`: small delivery receipts; PDFs have already been deleted.

At least `min_free_bytes` must remain before capture starts. A UPS and durable storage are still recommended for the Pi because no software can make an SD card or a partially scanned physical page power-loss-proof.
