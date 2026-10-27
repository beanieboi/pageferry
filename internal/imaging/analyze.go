package imaging

import "math"

type Class string

const (
	BW    Class = "bw"
	Gray  Class = "gray"
	Color Class = "color"
)

// Thresholds are areas on the page in square millimeters, so they do not
// depend on the scan resolution.
type Thresholds struct {
	// A page with less content than this is blank, unless it has color.
	// Fold creases, the shadow along the paper edge, dust and isolated
	// specks are not content.
	BlankContentMM2 float64
	// A page with more colored area than this is kept in color.
	ColorMM2 float64
	// A page without color but with more evenly gray area than this
	// (photos, shading) is kept in grayscale instead of black and white.
	GrayMM2 float64
}

var DefaultThresholds = Thresholds{BlankContentMM2: 100, ColorMM2: 15, GrayMM2: 400}

type Analysis struct {
	Blank bool
	// Class is how the page is stored, also when it is blank.
	Class Class
	// Paper is the luminance of the paper, Threshold the luminance below
	// which a pixel is black when the page is reduced to black and white.
	Paper, Threshold uint8
	// ContentMM2, ColorMM2 and GrayMM2 are the measured areas that the
	// Thresholds are compared to.
	ContentMM2, ColorMM2, GrayMM2 float64
}

const (
	// Analysis works on cells of about 1/75 inch (0.34 mm).
	cellsPerInch = 75
	// The paper edge and its shadow lie in this margin, punch holes (12 mm
	// from the edge) in the wider one along the sides.
	marginMM, sideMarginMM = 8, 16
	// Ink is darker than this fraction of the paper's luminance: pencil and
	// faded receipts are, bleed-through from the other side is not.
	inkFactor = 0.7
	// The black and white threshold lies within these fractions of the
	// paper's luminance.
	thresholdMin, thresholdMax = 0.5, 0.75
	// A cell is inked when at least this many of its pixels are ink, so
	// that single specks of noise do not count.
	minInkPixels = 2
	// Chroma (max-min of a cell's mean RGB) from which a cell counts as
	// colored. Scanners' color fringes along black text stay below it once
	// averaged over a cell.
	colorChroma = 40
	// A cell is evenly gray when all its pixels lie between these limits;
	// the upper one is relative to the paper. Only gray cells surrounded by
	// gray cells count, which leaves out the strokes of thin or light text.
	grayMin, grayBelowPaper = 40, 48
	// Components of inked cells whose mean thickness (cells per length) is
	// below this (mm) and that are at least lineAspect times as long are
	// lines, like fold creases. Words are 1.3 mm thick or more.
	lineMaxThicknessMM = 1
	lineAspect         = 8
	// Components smaller than this (mm²) are dust.
	minComponentMM2 = 2
)

// Analyze classifies a page scanned at dpi.
func Analyze(img *Image, dpi int, t Thresholds) Analysis {
	var hist [256]int
	img.eachLuminance(func(_, _ int, l uint8) { hist[l]++ })
	paper := percentile(&hist, img.Width*img.Height, 0.9)
	ink := uint8(float64(paper) * inkFactor)
	a := Analysis{Class: BW, Paper: paper, Threshold: otsu(&hist, uint8(float64(paper)*thresholdMin), uint8(float64(paper)*thresholdMax))}

	cell := int(math.Round(float64(dpi) / cellsPerInch))
	if cell < 1 {
		cell = 1
	}
	cw, ch := img.Width/cell, img.Height/cell
	cells := func(mm float64) int { return int(math.Round(mm / 25.4 * float64(dpi) / float64(cell))) }
	margin, side := cells(marginMM), cells(sideMarginMM)
	if cw <= 2*side || ch <= 2*margin {
		a.Blank = true
		return a
	}
	cellMM := float64(cell) * 25.4 / float64(dpi)
	cellMM2 := cellMM * cellMM
	grayMax := int(paper) - grayBelowPaper

	inked := make([]bool, cw*ch)
	gray := make([]bool, cw*ch)
	inkCount := make([]uint16, cw)
	minLum := make([]uint8, cw)
	maxLum := make([]uint8, cw)
	sums := make([]int, 3*cw)
	colorCells := 0
	for cy := margin; cy < ch-margin; cy++ {
		clear(inkCount)
		clear(sums)
		for i := range minLum {
			minLum[i], maxLum[i] = 255, 0
		}
		for y := cy * cell; y < (cy+1)*cell; y++ {
			row := img.Pix[y*img.Width*img.Channels:]
			for cx := side; cx < cw-side; cx++ {
				for x := cx * cell; x < (cx+1)*cell; x++ {
					var l uint8
					if img.Channels == 3 {
						r, g, b := row[3*x], row[3*x+1], row[3*x+2]
						sums[3*cx] += int(r)
						sums[3*cx+1] += int(g)
						sums[3*cx+2] += int(b)
						l = luminance(r, g, b)
					} else {
						l = row[x]
					}
					if l < ink {
						inkCount[cx]++
					}
					minLum[cx] = min(minLum[cx], l)
					maxLum[cx] = max(maxLum[cx], l)
				}
			}
		}
		n := cell * cell
		for cx := side; cx < cw-side; cx++ {
			if inkCount[cx] >= minInkPixels {
				inked[cy*cw+cx] = true
			}
			if img.Channels == 3 {
				r, g, b := sums[3*cx]/n, sums[3*cx+1]/n, sums[3*cx+2]/n
				if max(r, g, b)-min(r, g, b) >= colorChroma {
					colorCells++
				}
			}
			if int(minLum[cx]) >= grayMin && int(maxLum[cx]) <= grayMax {
				gray[cy*cw+cx] = true
			}
		}
	}
	a.ContentMM2 = float64(contentCells(inked, cw, ch, cellMM)) * cellMM2
	a.ColorMM2 = float64(colorCells) * cellMM2
	a.GrayMM2 = float64(grayAreaCells(gray, cw, ch)) * cellMM2
	switch {
	case a.ColorMM2 > t.ColorMM2:
		a.Class = Color
	case a.GrayMM2 > t.GrayMM2:
		a.Class = Gray
	}
	// Faint colored writing, like a note in colored pencil, is too light to
	// be ink but still content.
	a.Blank = a.ContentMM2 < t.BlankContentMM2 && a.Class != Color
	return a
}

func grayAreaCells(gray []bool, w, h int) int {
	n := 0
	for y := 1; y < h-1; y++ {
	cells:
		for x := 1; x < w-1; x++ {
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					if !gray[(y+dy)*w+x+dx] {
						continue cells
					}
				}
			}
			n++
		}
	}
	return n
}

// contentCells counts the inked cells in connected components that are
// neither lines nor dust.
func contentCells(inked []bool, w, h int, cellMM float64) int {
	maxThickness := lineMaxThicknessMM / cellMM
	minCells := int(math.Ceil(minComponentMM2 / (cellMM * cellMM)))
	total := 0
	var stack []int
	for start, on := range inked {
		if !on {
			continue
		}
		inked[start] = false
		stack = append(stack[:0], start)
		cells := 0
		x0, y0, x1, y1 := w, h, -1, -1
		for len(stack) > 0 {
			i := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			x, y := i%w, i/w
			cells++
			x0, y0, x1, y1 = min(x0, x), min(y0, y), max(x1, x), max(y1, y)
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					nx, ny := x+dx, y+dy
					if nx >= 0 && ny >= 0 && nx < w && ny < h && inked[ny*w+nx] {
						inked[ny*w+nx] = false
						stack = append(stack, ny*w+nx)
					}
				}
			}
		}
		long := float64(max(x1-x0, y1-y0) + 1)
		thickness := float64(cells) / long
		if cells < minCells || (thickness <= maxThickness && long >= lineAspect*thickness) {
			continue
		}
		total += cells
	}
	return total
}

func (img *Image) eachLuminance(f func(x, y int, l uint8)) {
	for y := 0; y < img.Height; y++ {
		row := img.Pix[y*img.Width*img.Channels : (y+1)*img.Width*img.Channels]
		for x := 0; x < img.Width; x++ {
			if img.Channels == 3 {
				f(x, y, luminance(row[3*x], row[3*x+1], row[3*x+2]))
			} else {
				f(x, y, row[x])
			}
		}
	}
}

func luminance(r, g, b uint8) uint8 {
	return uint8((77*int(r) + 150*int(g) + 29*int(b)) >> 8)
}

func percentile(hist *[256]int, total int, p float64) uint8 {
	target := int(float64(total) * p)
	seen := 0
	for v, n := range hist {
		seen += n
		if seen > target {
			return uint8(v)
		}
	}
	return 255
}

// otsu returns the threshold that best separates ink from paper, limited to
// lo..hi so that a page of almost only paper or only ink still gets a
// sensible one.
func otsu(hist *[256]int, lo, hi uint8) uint8 {
	var total, sum float64
	for v, n := range hist {
		total += float64(n)
		sum += float64(v * n)
	}
	var best float64
	threshold := lo
	var wB, sumB float64
	for v := 0; v < 256; v++ {
		wB += float64(hist[v])
		sumB += float64(v * hist[v])
		wF := total - wB
		if wB == 0 || wF == 0 {
			continue
		}
		mB, mF := sumB/wB, (sum-sumB)/wF
		if between := wB * wF * (mB - mF) * (mB - mF); between > best {
			best, threshold = between, uint8(v+1)
		}
	}
	return min(max(threshold, lo), hi)
}
