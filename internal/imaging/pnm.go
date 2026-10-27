// Package imaging analyzes scanned pages and prepares them for the PDF:
// blank page detection, color detection, downsampling and thresholding.
// Pages arrive from scanimage as PNM and leave as uncompressed TIFF for
// tiffcp and tiff2pdf.
package imaging

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
)

// Image is an 8-bit grayscale (Channels 1) or RGB (Channels 3) raster.
// Bilevel scans are expanded to grayscale with 0 for black and 255 for white.
type Image struct {
	Width, Height int
	Channels      int
	Pix           []byte
}

func ReadPNMFile(path string) (*Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, err := ReadPNM(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return img, nil
}

// ReadPNM reads a binary PBM (P4), PGM (P5) or PPM (P6) image with a maximum
// sample value of 255, as written by scanimage --format=pnm.
func ReadPNM(r io.Reader) (*Image, error) {
	br := bufio.NewReaderSize(r, 1<<16)
	magic, err := pnmToken(br)
	if err != nil {
		return nil, fmt.Errorf("read PNM header: %w", err)
	}
	channels := 0
	switch magic {
	case "P4":
	case "P5":
		channels = 1
	case "P6":
		channels = 3
	default:
		return nil, fmt.Errorf("unsupported PNM type %q", magic)
	}
	width, err := pnmNumber(br)
	if err != nil {
		return nil, err
	}
	height, err := pnmNumber(br)
	if err != nil {
		return nil, err
	}
	if width <= 0 || height <= 0 || width > 1<<16 || height > 1<<16 {
		return nil, fmt.Errorf("invalid PNM dimensions %dx%d", width, height)
	}
	if channels > 0 {
		maxval, err := pnmNumber(br)
		if err != nil {
			return nil, err
		}
		if maxval != 255 {
			return nil, fmt.Errorf("unsupported PNM maximum value %d", maxval)
		}
	}
	// Exactly one whitespace byte separates the header from the raster.
	if _, err := br.ReadByte(); err != nil {
		return nil, fmt.Errorf("read PNM header: %w", err)
	}
	if channels == 0 {
		return readPBMRaster(br, width, height)
	}
	img := &Image{Width: width, Height: height, Channels: channels, Pix: make([]byte, width*height*channels)}
	if _, err := io.ReadFull(br, img.Pix); err != nil {
		return nil, fmt.Errorf("read PNM raster: %w", err)
	}
	return img, nil
}

func readPBMRaster(r io.Reader, width, height int) (*Image, error) {
	img := &Image{Width: width, Height: height, Channels: 1, Pix: make([]byte, width*height)}
	row := make([]byte, (width+7)/8)
	for y := 0; y < height; y++ {
		if _, err := io.ReadFull(r, row); err != nil {
			return nil, fmt.Errorf("read PBM raster: %w", err)
		}
		out := img.Pix[y*width : (y+1)*width]
		for x := range out {
			if row[x>>3]&(0x80>>(x&7)) == 0 {
				out[x] = 255
			}
		}
	}
	return img, nil
}

func pnmToken(r *bufio.Reader) (string, error) {
	var token []byte
	for {
		b, err := r.ReadByte()
		if err != nil {
			if len(token) > 0 && errors.Is(err, io.EOF) {
				return "", io.ErrUnexpectedEOF
			}
			return "", err
		}
		switch {
		case b == '#' && len(token) == 0:
			if _, err := r.ReadString('\n'); err != nil {
				return "", err
			}
		case b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\v' || b == '\f':
			if len(token) > 0 {
				return string(token), r.UnreadByte()
			}
		default:
			if len(token) >= 16 {
				return "", errors.New("PNM header token too long")
			}
			token = append(token, b)
		}
	}
}

func pnmNumber(r *bufio.Reader) (int, error) {
	token, err := pnmToken(r)
	if err != nil {
		return 0, fmt.Errorf("read PNM header: %w", err)
	}
	n := 0
	for _, c := range []byte(token) {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("invalid PNM header number %q", token)
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}
