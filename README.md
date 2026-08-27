# PageFerry

PageFerry turns the ScanSnap button into a reliable, image-only PDF handoff to Paperless-ngx. It is one Go binary with two roles:

- `pageferry-button` (a symlink to the binary) is invoked by `scanbd`. It drains the ADF, builds and validates one PDF, and atomically publishes it to a durable local queue.
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

The binary uses only the Go standard library. Scanner acquisition and PDF construction deliberately use the mature system tools already required by `scanbd`: `scanimage`, `tiffcp`, `tiff2pdf`, and `pdfinfo`. No shell is involved.

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
- `mode`: usually `Color`, `Gray`, or `Lineart`, depending on the SANE backend.
- `pdf_quality`: JPEG quality embedded in the image-only PDF.
- `scan_options`: backend-specific SANE options. The supplied iX1300 example preserves the settings validated on this scanner.

One button press drains every sheet currently in the ADF into one multi-page PDF. Duplex produces two pages per sheet, including blank backs unless the scanner backend itself suppresses them. PageFerry does no OCR; Paperless is responsible for OCR and archival processing.

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
- `failed-captures/`: raw TIFF pages retained when PDF construction fails.
- `completed/`: small delivery receipts; PDFs have already been deleted.

At least `min_free_bytes` must remain before capture starts. A UPS and durable storage are still recommended for the Pi because no software can make an SD card or a partially scanned physical page power-loss-proof.
