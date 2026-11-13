package proto

import (
	"runtime"
	"sync"
)

// Video codecs understand "NV12" pictures (brightness at full resolution,
// colour at half resolution), not the BGRA pixels Windows draws with. These
// convert between the two using the BT.709 "video range" formulas. Both ends
// of the connection use this same code, so the colours round-trip.

// parallelRows splits rows [0,h) into bands, two rows at a time, and runs fn
// on each band concurrently.
func parallelRows(h int, fn func(y0, y1 int)) {
	n := runtime.NumCPU()
	band := ((h/n + 1) + 1) &^ 1
	if band < 32 {
		band = 32
	}
	var wg sync.WaitGroup
	for y := 0; y < h; y += band {
		wg.Add(1)
		go func(y0, y1 int) {
			defer wg.Done()
			fn(y0, y1)
		}(y, min(y+band, h))
	}
	wg.Wait()
}

// NV12Size is the number of bytes in a w×h NV12 picture with no row padding.
func NV12Size(w, h int) int { return w * h * 3 / 2 }

// BGRAToNV12 converts the top-left w×h (both even) of a BGRA frame whose rows
// are srcW pixels wide into dst, which must hold NV12Size(w, h) bytes.
func BGRAToNV12(dst, src []byte, srcW, w, h int) {
	parallelRows(h, func(y0, y1 int) {
		for y := y0; y < y1; y += 2 {
			r0 := src[y*srcW*4 : y*srcW*4+w*4]
			r1 := src[(y+1)*srcW*4 : (y+1)*srcW*4+w*4]
			d0 := dst[y*w : y*w+w]
			d1 := dst[(y+1)*w : (y+1)*w+w]
			uv := dst[w*h+(y/2)*w : w*h+(y/2)*w+w]
			for x := 0; x < w; x += 2 {
				p := x * 4
				b0, g0, c0 := int(r0[p]), int(r0[p+1]), int(r0[p+2])
				b1, g1, c1 := int(r0[p+4]), int(r0[p+5]), int(r0[p+6])
				b2, g2, c2 := int(r1[p]), int(r1[p+1]), int(r1[p+2])
				b3, g3, c3 := int(r1[p+4]), int(r1[p+5]), int(r1[p+6])
				d0[x] = byte((47*c0+157*g0+16*b0+128)>>8 + 16)
				d0[x+1] = byte((47*c1+157*g1+16*b1+128)>>8 + 16)
				d1[x] = byte((47*c2+157*g2+16*b2+128)>>8 + 16)
				d1[x+1] = byte((47*c3+157*g3+16*b3+128)>>8 + 16)
				b, g, r := b0+b1+b2+b3, g0+g1+g2+g3, c0+c1+c2+c3 // sums of the 2×2 block
				uv[x] = byte((-26*r-87*g+112*b+512)>>10 + 128)
				uv[x+1] = byte((112*r-102*g-10*b+512)>>10 + 128)
			}
		}
	})
}

func clamp8(v int) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}

// NV12ToBGRA converts a decoded picture into the top-left w×h of a BGRA
// frame whose rows are dstW pixels wide. yPlane and uvPlane have rows of
// stride bytes (decoders pad rows).
func NV12ToBGRA(dst []byte, dstW int, yPlane, uvPlane []byte, stride, w, h int) {
	w &^= 1
	h &^= 1
	parallelRows(h, func(y0, y1 int) {
		for y := y0; y < y1; y++ {
			ys := yPlane[y*stride : y*stride+w]
			uvs := uvPlane[(y/2)*stride : (y/2)*stride+w]
			d := dst[y*dstW*4 : y*dstW*4+w*4]
			for x := 0; x < w; x += 2 {
				u, v := int(uvs[x])-128, int(uvs[x+1])-128
				rv, gv, bv := 459*v+128, -55*u-136*v+128, 541*u+128
				c := 298 * (int(ys[x]) - 16)
				p := x * 4
				d[p], d[p+1], d[p+2], d[p+3] = clamp8((c+bv)>>8), clamp8((c+gv)>>8), clamp8((c+rv)>>8), 255
				c = 298 * (int(ys[x+1]) - 16)
				d[p+4], d[p+5], d[p+6], d[p+7] = clamp8((c+bv)>>8), clamp8((c+gv)>>8), clamp8((c+rv)>>8), 255
			}
		}
	})
}

// PlaneLayout works out how a decoder laid out an NV12 picture in a block of
// n bytes: the length of each row (stride) and how many rows the brightness
// plane has before the colour plane starts. Decoders pad both, and graphics
// cards pad differently from software, so the sizes the decoder announced
// (the hints) are checked against the block's actual length.
func PlaneLayout(n, width, height, strideHint, rowsHint int) (stride, rows int, ok bool) {
	fits := func(s, r int) bool { return s >= width && r >= height && s*r*3/2 == n }
	if fits(strideHint, rowsHint) {
		return strideHint, rowsHint, true
	}
	candidates := []int{strideHint, width}
	for _, a := range []int{16, 32, 64, 128, 256} {
		candidates = append(candidates, (width+a-1)/a*a)
	}
	for _, s := range candidates {
		if s < width || n*2%(3*s) != 0 {
			continue
		}
		if r := n * 2 / (3 * s); r >= height && r <= height+512 && r%2 == 0 {
			return s, r, true
		}
	}
	// Longer than expected in some other way: trust the hints if they fit.
	if strideHint >= width && rowsHint >= height && strideHint*rowsHint*3/2 <= n {
		return strideHint, rowsHint, true
	}
	return 0, 0, false
}

// NV12RectsToBGRA is NV12ToBGRA limited to the given areas of the picture,
// leaving everything else in dst untouched. Areas are clipped to w×h and
// must start on even coordinates (screen tiles do).
func NV12RectsToBGRA(dst []byte, dstW int, yPlane, uvPlane []byte, stride, w, h int, rects []Rect) {
	w &^= 1
	h &^= 1
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.NumCPU())
	for _, r := range rects {
		if r.X < 0 || r.Y < 0 || r.X&1 != 0 || r.Y&1 != 0 || r.X >= w || r.Y >= h {
			continue
		}
		x1, y1 := min(r.X+r.W, w)&^1, min(r.Y+r.H, h)&^1
		if x1 <= r.X || y1 <= r.Y {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(x0, y0 int) {
			defer func() { <-sem; wg.Done() }()
			for y := y0; y < y1; y++ {
				ys := yPlane[y*stride+x0 : y*stride+x1]
				uvs := uvPlane[(y/2)*stride+x0 : (y/2)*stride+x1]
				d := dst[(y*dstW+x0)*4 : (y*dstW+x1)*4]
				for x := 0; x < x1-x0; x += 2 {
					u, v := int(uvs[x])-128, int(uvs[x+1])-128
					rv, gv, bv := 459*v+128, -55*u-136*v+128, 541*u+128
					c := 298 * (int(ys[x]) - 16)
					p := x * 4
					d[p], d[p+1], d[p+2], d[p+3] = clamp8((c+bv)>>8), clamp8((c+gv)>>8), clamp8((c+rv)>>8), 255
					c = 298 * (int(ys[x+1]) - 16)
					d[p+4], d[p+5], d[p+6], d[p+7] = clamp8((c+bv)>>8), clamp8((c+gv)>>8), clamp8((c+rv)>>8), 255
				}
			}
		}(r.X, r.Y)
	}
	wg.Wait()
}
