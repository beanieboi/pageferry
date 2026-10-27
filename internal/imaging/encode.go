package imaging

import (
	"bufio"
	"encoding/binary"
	"io"
	"math"
	"os"
)

// Gray returns the image's luminance.
func (img *Image) Gray() *Image {
	if img.Channels == 1 {
		return img
	}
	out := &Image{Width: img.Width, Height: img.Height, Channels: 1, Pix: make([]byte, img.Width*img.Height)}
	for i := range out.Pix {
		out.Pix[i] = luminance(img.Pix[3*i], img.Pix[3*i+1], img.Pix[3*i+2])
	}
	return out
}

// Whiten maps everything at least as light as just below the paper to
// white and stretches the rest, which removes most of the paper's texture
// and noise that JPEG would otherwise spend its bytes on.
func (img *Image) Whiten(paper uint8) {
	white := max(int(paper)-whitenBelowPaper, 128)
	var lut [256]byte
	for v := range lut {
		lut[v] = byte(min(255, (v*255+white/2)/white))
	}
	for i, v := range img.Pix {
		img.Pix[i] = lut[v]
	}
}

const whitenBelowPaper = 10

// Downscale returns the image reduced to width x height by averaging the
// area each output pixel covers. It returns img unchanged unless that makes
// it smaller and no side longer.
func (img *Image) Downscale(width, height int) *Image {
	if width > img.Width || height > img.Height || width*height == img.Width*img.Height || width <= 0 || height <= 0 {
		return img
	}
	c := img.Channels
	xs, ys := areaWeights(img.Width, width), areaWeights(img.Height, height)
	out := &Image{Width: width, Height: height, Channels: c, Pix: make([]byte, width*height*c)}
	row := make([]float32, width*c)
	acc := make([]float32, width*c)
	for oy, sources := range ys {
		clear(acc)
		for _, sy := range sources {
			src := img.Pix[sy.i*img.Width*c : (sy.i+1)*img.Width*c]
			for ox, sxs := range xs {
				for k := 0; k < c; k++ {
					var v float32
					for _, sx := range sxs {
						v += float32(src[sx.i*c+k]) * sx.w
					}
					row[ox*c+k] = v
				}
			}
			for i, v := range row {
				acc[i] += v * sy.w
			}
		}
		dst := out.Pix[oy*width*c : (oy+1)*width*c]
		for i, v := range acc {
			dst[i] = uint8(min(255, v+0.5))
		}
	}
	return out
}

type weight struct {
	i int
	w float32
}

func areaWeights(src, dst int) [][]weight {
	scale := float64(src) / float64(dst)
	out := make([][]weight, dst)
	for o := range out {
		lo, hi := float64(o)*scale, float64(o+1)*scale
		for i := int(lo); i < src && float64(i) < hi; i++ {
			if a := math.Min(hi, float64(i+1)) - math.Max(lo, float64(i)); a > 1e-9 {
				out[o] = append(out[o], weight{i, float32(a / scale)})
			}
		}
	}
	return out
}

// Bilevel is a black and white raster, one bit per pixel with 1 for black,
// each row padded to whole bytes.
type Bilevel struct {
	Width, Height int
	Bits          []byte
}

// Threshold makes pixels darker than threshold black.
func (img *Image) Threshold(threshold uint8) *Bilevel {
	stride := (img.Width + 7) / 8
	out := &Bilevel{Width: img.Width, Height: img.Height, Bits: make([]byte, stride*img.Height)}
	img.eachLuminance(func(x, y int, l uint8) {
		if l < threshold {
			out.Bits[y*stride+x>>3] |= 0x80 >> (x & 7)
		}
	})
	return out
}

// WriteTIFFFile writes the image as an uncompressed single-page TIFF.
func (img *Image) WriteTIFFFile(path string, dpi float64) error {
	photometric := uint16(1) // BlackIsZero
	if img.Channels == 3 {
		photometric = 2 // RGB
	}
	return writeTIFFFile(path, img.Width, img.Height, img.Channels, 8, photometric, dpi, img.Pix)
}

// WriteTIFFFile writes the raster as an uncompressed single-page TIFF.
func (b *Bilevel) WriteTIFFFile(path string, dpi float64) error {
	return writeTIFFFile(path, b.Width, b.Height, 1, 1, 0 /* WhiteIsZero */, dpi, b.Bits)
}

func writeTIFFFile(path string, width, height, samples, bits int, photometric uint16, dpi float64, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 1<<16)
	if err := writeTIFF(w, width, height, samples, bits, photometric, dpi, data); err != nil {
		f.Close()
		return err
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// writeTIFF writes a little-endian baseline TIFF with one strip: the header,
// the IFD, the values that do not fit into it, then the raster.
func writeTIFF(w io.Writer, width, height, samples, bits int, photometric uint16, dpi float64, data []byte) error {
	const (
		short    = 3
		long     = 4
		rational = 5
		entries  = 14
		ifd      = 8
		extra    = ifd + 2 + entries*12 + 4
	)
	resolution := [2]uint32{uint32(math.Round(dpi * 1000)), 1000}
	bitsOffset := uint32(extra)
	resOffset := bitsOffset + 6
	dataOffset := resOffset + 8
	type entry struct {
		tag, typ uint16
		count    uint32
		value    uint32
	}
	bitsEntry := entry{258, short, 1, uint32(bits)}
	if samples == 3 {
		bitsEntry = entry{258, short, 3, bitsOffset}
	}
	ifdEntries := [entries]entry{
		{256, long, 1, uint32(width)},
		{257, long, 1, uint32(height)},
		bitsEntry,
		{259, short, 1, 1}, // no compression
		{262, short, 1, uint32(photometric)},
		{273, long, 1, dataOffset},
		{274, short, 1, 1}, // top left
		{277, short, 1, uint32(samples)},
		{278, long, 1, uint32(height)},
		{279, long, 1, uint32(len(data))},
		{282, rational, 1, resOffset},
		{283, rational, 1, resOffset},
		{284, short, 1, 1}, // chunky
		{296, short, 1, 2}, // inch
	}
	buf := make([]byte, 0, dataOffset)
	le := binary.LittleEndian
	buf = append(buf, 'I', 'I')
	buf = le.AppendUint16(buf, 42)
	buf = le.AppendUint32(buf, ifd)
	buf = le.AppendUint16(buf, entries)
	for _, e := range ifdEntries {
		buf = le.AppendUint16(buf, e.tag)
		buf = le.AppendUint16(buf, e.typ)
		buf = le.AppendUint32(buf, e.count)
		if e.typ == short && e.count == 1 {
			buf = le.AppendUint16(buf, uint16(e.value))
			buf = le.AppendUint16(buf, 0)
		} else {
			buf = le.AppendUint32(buf, e.value)
		}
	}
	buf = le.AppendUint32(buf, 0) // no next IFD
	for range 3 {
		buf = le.AppendUint16(buf, uint16(bits))
	}
	buf = le.AppendUint32(buf, resolution[0])
	buf = le.AppendUint32(buf, resolution[1])
	if _, err := w.Write(buf); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}
