package imaging

import (
	"bytes"
	"encoding/binary"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
)

const dpi = 300

// a4 returns a white A4 page at 300 DPI with a little sensor noise.
func a4() *Image {
	img := &Image{Width: 2480, Height: 3508, Channels: 3}
	img.Pix = make([]byte, img.Width*img.Height*3)
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range img.Pix {
		img.Pix[i] = byte(244 + rng.IntN(8))
	}
	return img
}

func (img *Image) fill(x0, y0, x1, y1 int, r, g, b byte) {
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			i := (y*img.Width + x) * 3
			img.Pix[i], img.Pix[i+1], img.Pix[i+2] = r, g, b
		}
	}
}

// text draws lines of words: blocks of 3 px strokes, 30 px tall.
func (img *Image) text(lines int) {
	for line := 0; line < lines; line++ {
		y := 500 + line*60
		for word := 0; word < 12; word++ {
			x := 300 + word*150
			for stroke := 0; stroke < 18; stroke++ {
				img.fill(x+stroke*6, y, x+stroke*6+3, y+30, 20, 20, 20)
			}
		}
	}
}

// blankBack is the back of a folded sheet: crease shadows, the paper edge
// and dust.
func blankBack() *Image {
	img := a4()
	img.fill(0, 1169, img.Width, 1172, 90, 90, 90)
	img.fill(0, 2338, img.Width/2, 2340, 60, 60, 60)
	img.fill(0, 0, 25, img.Height, 30, 30, 30)
	img.fill(0, img.Height-30, img.Width, img.Height, 30, 30, 30)
	rng := rand.New(rand.NewPCG(3, 4))
	for range 400 {
		x, y := 200+rng.IntN(2000), 200+rng.IntN(3000)
		img.fill(x, y, x+2, y+2, 0, 0, 0)
	}
	return img
}

func TestAnalyze(t *testing.T) {
	text := a4()
	text.text(3)

	logo := a4()
	logo.text(3)
	logo.fill(1900, 250, 2018, 368, 230, 0, 0) // 10 x 10 mm

	photo := a4()
	photo.text(3)
	for y := 1500; y < 2200; y++ {
		for x := 400; x < 1400; x++ {
			v := byte(60 + (x+y)%120)
			photo.fill(x, y, x+1, y+1, v, v, v)
		}
	}

	for _, test := range []struct {
		name  string
		img   *Image
		blank bool
		class Class
	}{
		{"blank back", blankBack(), true, BW},
		{"three lines of text", text, false, BW},
		{"colored logo", logo, false, Color},
		{"photo", photo, false, Gray},
		{"grayscale scan", text.Gray(), false, BW},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := Analyze(test.img, dpi, DefaultThresholds)
			if a.Blank != test.blank || a.Class != test.class {
				t.Fatalf("got blank=%v class=%s, want blank=%v class=%s (%+v)", a.Blank, a.Class, test.blank, test.class, a)
			}
			if a.Threshold < 100 || a.Threshold > 200 {
				t.Fatalf("threshold %d", a.Threshold)
			}
		})
	}
}

func TestReadPNM(t *testing.T) {
	for _, test := range []struct {
		name     string
		data     string
		channels int
		pix      []byte
	}{
		{"pbm", "P4\n# comment\n10 2\n\x80\x40\xff\xc0", 1, []byte{0, 255, 255, 255, 255, 255, 255, 255, 255, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}},
		{"pgm", "P5 2 1 255\n\x00\x80", 1, []byte{0, 128}},
		{"ppm", "P6\n1 1\n255\n\x01\x02\x03", 3, []byte{1, 2, 3}},
	} {
		t.Run(test.name, func(t *testing.T) {
			img, err := ReadPNM(bytes.NewReader([]byte(test.data)))
			if err != nil {
				t.Fatal(err)
			}
			if img.Channels != test.channels || !bytes.Equal(img.Pix, test.pix) {
				t.Fatalf("got %d channels %v", img.Channels, img.Pix)
			}
		})
	}
	for _, data := range []string{"P6\n1 1\n65535\n\x00\x00\x00\x00\x00\x00", "P6\n2 2\n255\n\x00", "P3\n1 1\n255\n0 0 0"} {
		if _, err := ReadPNM(bytes.NewReader([]byte(data))); err == nil {
			t.Fatalf("accepted %q", data)
		}
	}
}

func TestDownscale(t *testing.T) {
	img := &Image{Width: 3, Height: 1, Channels: 1, Pix: []byte{0, 90, 180}}
	got := img.Downscale(2, 1)
	// The middle pixel is split evenly between both output pixels.
	if got.Width != 2 || got.Pix[0] != 30 || got.Pix[1] != 150 {
		t.Fatalf("got %dx%d %v", got.Width, got.Height, got.Pix)
	}
	if img.Downscale(3, 1) != img {
		t.Fatal("did not return the image unchanged")
	}
}

func TestThreshold(t *testing.T) {
	img := &Image{Width: 9, Height: 1, Channels: 1, Pix: []byte{0, 255, 99, 100, 255, 255, 255, 255, 50}}
	got := img.Threshold(100)
	if !bytes.Equal(got.Bits, []byte{0b10100000, 0b10000000}) {
		t.Fatalf("got %08b", got.Bits)
	}
}

func TestWhiten(t *testing.T) {
	img := &Image{Width: 4, Height: 1, Channels: 1, Pix: []byte{0, 115, 230, 250}}
	img.Whiten(240)
	if !bytes.Equal(img.Pix, []byte{0, 128, 255, 255}) {
		t.Fatalf("got %v", img.Pix)
	}
}

func TestWriteTIFF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "page.tif")
	img := &Image{Width: 2, Height: 2, Channels: 3, Pix: bytes.Repeat([]byte{7}, 12)}
	if err := img.WriteTIFFFile(path, 200); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	le := binary.LittleEndian
	if string(data[:4]) != "II*\x00" || le.Uint32(data[4:]) != 8 {
		t.Fatalf("bad header %q", data[:8])
	}
	tags := map[uint16]uint32{}
	for i := 0; i < int(le.Uint16(data[8:])); i++ {
		e := data[10+12*i:]
		tags[le.Uint16(e)] = le.Uint32(e[8:])
	}
	offset, length := tags[273], tags[279]
	if tags[256] != 2 || tags[257] != 2 || tags[277] != 3 || length != 12 || int(offset+length) != len(data) {
		t.Fatalf("bad tags %v for %d bytes", tags, len(data))
	}
	res := tags[282]
	if le.Uint32(data[res:])/le.Uint32(data[res+4:]) != 200 {
		t.Fatal("bad resolution")
	}
	bits := tags[258]
	if le.Uint16(data[bits:]) != 8 || le.Uint16(data[bits+4:]) != 8 {
		t.Fatal("bad bits per sample")
	}
}
