package proto

import (
	"bytes"
	"compress/flate"
	"errors"
	"image"
	"image/jpeg"
	"io"
	"sync"
)

// Frames are 32-bit BGRA, top-down, stride = width*4 (a Windows DIB section).

type Rect struct{ X, Y, W, H int }

const (
	EncJPEG  = 0
	EncFlate = 1 // deflated BGR, lossless; used for text and flat UI
)

// DirtyRects compares cur with prev in tile-sized blocks and returns the
// changed areas, merging neighbouring tiles in the same band. prev == nil
// means everything changed.
func DirtyRects(cur, prev []byte, w, h, tile int) []Rect {
	var out []Rect
	stride := w * 4
	for y := 0; y < h; y += tile {
		bh := min(tile, h-y)
		runStart := -1
		for x := 0; ; x += tile {
			dirty := false
			if x < w {
				bw := min(tile, w-x)
				if prev == nil {
					dirty = true
				} else {
					for r := y; r < y+bh; r++ {
						o := r*stride + x*4
						if !bytes.Equal(cur[o:o+bw*4], prev[o:o+bw*4]) {
							dirty = true
							break
						}
					}
				}
			}
			if dirty && runStart < 0 {
				runStart = x
			}
			if !dirty && runStart >= 0 {
				out = append(out, Rect{runStart, y, min(x, w) - runStart, bh})
				runStart = -1
			}
			if x >= w {
				break
			}
		}
	}
	return out
}

var flateWriters = sync.Pool{New: func() any {
	fw, _ := flate.NewWriter(nil, flate.BestSpeed)
	return fw
}}

// looksFlat reports whether the area is mostly runs of identical pixels,
// which is what text and UI look like and photos and video do not.
func looksFlat(fb []byte, fbW int, r Rect) bool {
	same, total := 0, 0
	for y := r.Y; y < r.Y+r.H; y += 2 {
		o := (y*fbW + r.X) * 4
		for x := 1; x < r.W; x++ {
			p := o + x*4
			if fb[p] == fb[p-4] && fb[p+1] == fb[p-3] && fb[p+2] == fb[p-2] {
				same++
			}
			total++
		}
	}
	return total == 0 || same*10 >= total*6
}

func EncodeRect(fb []byte, fbW int, r Rect, quality int) (byte, []byte) {
	var out bytes.Buffer
	if looksFlat(fb, fbW, r) {
		raw := make([]byte, r.W*r.H*3)
		i := 0
		for y := r.Y; y < r.Y+r.H; y++ {
			o := (y*fbW + r.X) * 4
			for x := 0; x < r.W; x++ {
				raw[i], raw[i+1], raw[i+2] = fb[o], fb[o+1], fb[o+2]
				i += 3
				o += 4
			}
		}
		fw := flateWriters.Get().(*flate.Writer)
		fw.Reset(&out)
		fw.Write(raw)
		fw.Close()
		flateWriters.Put(fw)
		return EncFlate, out.Bytes()
	}
	img := image.NewRGBA(image.Rect(0, 0, r.W, r.H))
	i := 0
	for y := r.Y; y < r.Y+r.H; y++ {
		o := (y*fbW + r.X) * 4
		for x := 0; x < r.W; x++ {
			img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = fb[o+2], fb[o+1], fb[o], 255
			i += 4
			o += 4
		}
	}
	jpeg.Encode(&out, img, &jpeg.Options{Quality: quality})
	return EncJPEG, out.Bytes()
}

// DecodeRect writes the decoded area into fb. Callers may decode disjoint
// rects concurrently.
func DecodeRect(enc byte, data []byte, r Rect, fb []byte, fbW, fbH int) error {
	if r.W <= 0 || r.H <= 0 || r.X < 0 || r.Y < 0 || r.X+r.W > fbW || r.Y+r.H > fbH {
		return errors.New("rect out of bounds")
	}
	switch enc {
	case EncFlate:
		raw := make([]byte, r.W*r.H*3)
		fr := flate.NewReader(bytes.NewReader(data))
		if _, err := io.ReadFull(fr, raw); err != nil {
			return err
		}
		i := 0
		for y := r.Y; y < r.Y+r.H; y++ {
			o := (y*fbW + r.X) * 4
			for x := 0; x < r.W; x++ {
				fb[o], fb[o+1], fb[o+2], fb[o+3] = raw[i], raw[i+1], raw[i+2], 255
				i += 3
				o += 4
			}
		}
		return nil
	case EncJPEG:
		img, err := jpeg.Decode(bytes.NewReader(data))
		if err != nil {
			return err
		}
		b := img.Bounds()
		if b.Dx() != r.W || b.Dy() != r.H {
			return errors.New("jpeg size mismatch")
		}
		for y := 0; y < r.H; y++ {
			o := ((r.Y+y)*fbW + r.X) * 4
			for x := 0; x < r.W; x++ {
				cr, cg, cb, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
				fb[o], fb[o+1], fb[o+2], fb[o+3] = byte(cb>>8), byte(cg>>8), byte(cr>>8), 255
				o += 4
			}
		}
		return nil
	}
	return errors.New("unknown encoding")
}
