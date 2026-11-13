package proto

import (
	"math/rand"
	"testing"
)

func TestYUVRoundTrip(t *testing.T) {
	srcW, w, h := 205, 200, 130 // source wider than the converted area, like an odd-sized screen
	src := make([]byte, srcW*h*4)
	// 2×2 blocks of one colour, so half-resolution colour loses nothing.
	for y := 0; y < h; y += 2 {
		for x := 0; x < srcW-1; x += 2 {
			b, g, r := byte(rand.Intn(256)), byte(rand.Intn(256)), byte(rand.Intn(256))
			switch (x/2 + y/2) % 7 {
			case 0:
				b, g, r = 0, 0, 0
			case 1:
				b, g, r = 255, 255, 255
			case 2:
				b, g, r = 0, 0, 255
			}
			for _, o := range []int{0, 4, srcW * 4, srcW*4 + 4} {
				p := (y*srcW+x)*4 + o
				src[p], src[p+1], src[p+2] = b, g, r
			}
		}
	}
	nv := make([]byte, NV12Size(w, h))
	BGRAToNV12(nv, src, srcW, w, h)

	// Decoders hand back padded rows: simulate a stride of 224.
	stride := 224
	yp, uvp := make([]byte, stride*h), make([]byte, stride*h/2)
	for y := 0; y < h; y++ {
		copy(yp[y*stride:], nv[y*w:y*w+w])
	}
	for y := 0; y < h/2; y++ {
		copy(uvp[y*stride:], nv[w*h+y*w:w*h+y*w+w])
	}
	dstW := 210
	dst := make([]byte, dstW*h*4)
	NV12ToBGRA(dst, dstW, yp, uvp, stride, w, h)
	worst := 0
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			for k := 0; k < 3; k++ {
				d := int(src[(y*srcW+x)*4+k]) - int(dst[(y*dstW+x)*4+k])
				if d < 0 {
					d = -d
				}
				worst = max(worst, d)
			}
			if dst[(y*dstW+x)*4+3] != 255 {
				t.Fatal("alpha")
			}
		}
	}
	if worst > 6 {
		t.Fatalf("colours drift by up to %d levels", worst)
	}
	if dst[(0*dstW+w)*4] != 0 {
		t.Fatal("wrote outside the picture")
	}
}

func BenchmarkBGRAToNV12(b *testing.B) {
	w, h := 2880, 1800
	src, dst := make([]byte, w*h*4), make([]byte, NV12Size(w, h))
	for i := 0; i < b.N; i++ {
		BGRAToNV12(dst, src, w, w, h)
	}
}

func BenchmarkNV12ToBGRA(b *testing.B) {
	w, h := 2880, 1800
	nv, dst := make([]byte, NV12Size(w, h)), make([]byte, w*h*4)
	for i := 0; i < b.N; i++ {
		NV12ToBGRA(dst, w, nv[:w*h], nv[w*h:], w, w, h)
	}
}

func TestPlaneLayout(t *testing.T) {
	cases := []struct{ n, w, h, sh, rh, s, r int }{
		{2880 * 1800 * 3 / 2, 2880, 1800, 2880, 1800, 2880, 1800}, // exact
		{2880 * 1808 * 3 / 2, 2880, 1800, 2880, 1800, 2880, 1808}, // extra rows the hints did not mention
		{1920 * 1088 * 3 / 2, 1920, 1080, 1920, 1088, 1920, 1088}, // hints already padded
		{2944 * 1824 * 3 / 2, 2880, 1800, 2880, 1808, 2944, 1824}, // rows and stride both padded
		{1366*768*3/2 + 100, 1366, 768, 1366, 768, 1366, 768},     // trailing slack
	}
	for i, c := range cases {
		s, r, ok := PlaneLayout(c.n, c.w, c.h, c.sh, c.rh)
		if !ok || s != c.s || r != c.r {
			t.Fatalf("case %d: got %d x %d (%v), want %d x %d", i, s, r, ok, c.s, c.r)
		}
	}
	if _, _, ok := PlaneLayout(1000, 2880, 1800, 2880, 1800); ok {
		t.Fatal("accepted a block that is too small")
	}
}

func TestNV12Rects(t *testing.T) {
	w, h, stride := 200, 130, 208
	yp, uvp := make([]byte, stride*h), make([]byte, stride*h/2)
	for i := range yp {
		yp[i] = byte(40 + i%150)
	}
	for i := range uvp {
		uvp[i] = byte(100 + i%60)
	}
	full := make([]byte, w*h*4)
	NV12ToBGRA(full, w, yp, uvp, stride, w, h)
	part := make([]byte, w*h*4)
	rects := []Rect{{64, 0, 64, 64}, {128, 64, 100, 100}, {0, 128, 64, 64}, {3, 0, 10, 10}, {400, 0, 10, 10}}
	NV12RectsToBGRA(part, w, yp, uvp, stride, w, h, rects)
	inside := func(x, y int) bool {
		return (x >= 64 && x < 128 && y < 64) || (x >= 128 && y >= 64) || (x < 64 && y >= 128)
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			o := (y*w + x) * 4
			if inside(x, y) {
				if part[o] != full[o] || part[o+1] != full[o+1] || part[o+2] != full[o+2] || part[o+3] != 255 {
					t.Fatalf("area pixel %d,%d differs from the full conversion", x, y)
				}
			} else if part[o]|part[o+1]|part[o+2]|part[o+3] != 0 {
				t.Fatalf("pixel %d,%d outside the areas was touched", x, y)
			}
		}
	}
}
