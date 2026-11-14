// Command winres gives a Windows program its icon and version details.
//
// Windows reads both from "resources" stored inside the .exe. The Go linker
// includes any file named *.syso that it finds in a program's folder, so
// this tool writes rsrc_windows_amd64.syso and rsrc_windows_arm64.syso
// there: small object files holding nothing but a resource section.
//
// The icon is drawn by code, so the repository needs no binary artwork.
//
//	go run ./tools/winres -dir cmd/deskbridge-client -version 1.0.0 \
//	    -desc "Deskbridge client" -file deskbridge-client.exe
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf16"
)

func main() {
	dir := flag.String("dir", "", "folder of the program to stamp")
	version := flag.String("version", "0.0.0", "version, as major.minor.patch")
	desc := flag.String("desc", "Deskbridge", "file description")
	file := flag.String("file", "", "name of the .exe")
	ico := flag.String("ico", "", "also write the icon to this .ico file")
	flag.Parse()
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "winres: -dir is required")
		os.Exit(2)
	}

	sizes := []int{16, 20, 24, 32, 40, 48, 64, 256}
	var images [][]byte
	for _, s := range sizes {
		images = append(images, iconImage(drawIcon(s)))
	}
	res := []resource{}
	group := []byte{0, 0, 1, 0, byte(len(sizes)), 0}
	for i, s := range sizes {
		res = append(res, resource{typ: 3, id: uint16(i + 1), data: images[i]}) // RT_ICON
		group = append(group, byte(s), byte(s), 0, 0, 1, 0, 32, 0)              // 256 wraps to 0, as the format wants
		group = binary.LittleEndian.AppendUint32(group, uint32(len(images[i])))
		group = binary.LittleEndian.AppendUint16(group, uint16(i+1))
	}
	res = append(res, resource{typ: 14, id: 1, data: group}) // RT_GROUP_ICON
	res = append(res, resource{typ: 16, id: 1, data: versionInfo(*version, *desc, *file)})

	for arch, m := range map[string]struct {
		machine uint16
		reloc   uint16
	}{"amd64": {0x8664, 3}, "arm64": {0xAA64, 2}} { // relocation: "address without image base"
		path := filepath.Join(*dir, "rsrc_windows_"+arch+".syso")
		if err := os.WriteFile(path, coff(res, m.machine, m.reloc), 0644); err != nil {
			fmt.Fprintln(os.Stderr, "winres:", err)
			os.Exit(1)
		}
	}
	if *ico != "" {
		os.WriteFile(*ico, icoFile(sizes, images), 0644)
	}
}

// ---------- the icon ----------

type rgb struct{ r, g, b float64 }

func hex(v uint32) rgb { return rgb{float64(v >> 16 & 255), float64(v >> 8 & 255), float64(v & 255)} }

func inRound(x, y, x0, y0, x1, y1, r float64) bool {
	if x < x0 || x > x1 || y < y0 || y > y1 {
		return false
	}
	cx, cy := math.Max(x0+r, math.Min(x, x1-r)), math.Max(y0+r, math.Min(y, y1-r))
	return (x-cx)*(x-cx)+(y-cy)*(y-cy) <= r*r
}

// colourAt returns the icon's colour at a point in the unit square, and
// whether the point is inside the icon at all. The design: a blue-to-teal
// rounded tile with a white monitor; on larger sizes a bridge spans the
// screen.
func colourAt(x, y float64, detailed bool) (rgb, bool) {
	if !inRound(x, y, 0.03, 0.03, 0.97, 0.97, 0.21) {
		return rgb{}, false
	}
	a, b := hex(0x2D6BF0), hex(0x10B3A3)
	t := (x + y) / 2
	c := rgb{a.r + (b.r-a.r)*t, a.g + (b.g-a.g)*t, a.b + (b.b-a.b)*t}
	white := hex(0xFFFFFF)

	// Monitor: larger and plainer on small sizes so it stays readable.
	fx0, fy0, fx1, fy1, fr, border := 0.17, 0.23, 0.83, 0.66, 0.06, 0.055
	if !detailed {
		fx0, fy0, fx1, fy1, fr, border = 0.13, 0.19, 0.87, 0.67, 0.07, 0.085
	}
	if inRound(x, y, 0.44, fy1-0.01, 0.56, 0.77, 0) || inRound(x, y, 0.31, 0.75, 0.69, 0.82, 0.035) {
		c = white // stand and base
	}
	if inRound(x, y, fx0, fy0, fx1, fy1, fr) {
		c = white
		sx0, sy0, sx1, sy1 := fx0+border, fy0+border, fx1-border, fy1-border
		if inRound(x, y, sx0, sy0, sx1, sy1, fr*0.5) {
			c = hex(0x12306B) // the screen
			if detailed {
				// A bridge: an arch standing on a deck.
				cx, base := 0.5, sy1-0.075
				d := math.Hypot((x-cx)/0.205, (y-base)/0.20)
				if y <= base && math.Abs(d-1) <= 0.10 {
					c = hex(0x8FD9FF)
				}
				if y >= base-0.012 && y <= base+0.028 && x >= sx0+0.03 && x <= sx1-0.03 {
					c = white
				}
			}
		}
	}
	return c, true
}

// drawIcon renders the icon at size×size, smoothing edges by sampling each
// pixel several times.
func drawIcon(size int) *image.NRGBA {
	const ss = 5
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			var r, g, b, n float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					x := (float64(px) + (float64(sx)+0.5)/ss) / float64(size)
					y := (float64(py) + (float64(sy)+0.5)/ss) / float64(size)
					if c, in := colourAt(x, y, size >= 32); in {
						r, g, b, n = r+c.r, g+c.g, b+c.b, n+1
					}
				}
			}
			if n > 0 {
				img.SetNRGBA(px, py, color.NRGBA{uint8(r/n + 0.5), uint8(g/n + 0.5), uint8(b/n + 0.5), uint8(n*255/(ss*ss) + 0.5)})
			}
		}
	}
	return img
}

// iconImage encodes one size the way icons store it: PNG for 256 pixels, a
// bottom-up 32-bit bitmap followed by an (unused) 1-bit mask otherwise.
func iconImage(img *image.NRGBA) []byte {
	size := img.Bounds().Dx()
	var out bytes.Buffer
	if size >= 256 {
		png.Encode(&out, img)
		return out.Bytes()
	}
	maskRow := (size + 31) / 32 * 4
	hdr := make([]byte, 40)
	binary.LittleEndian.PutUint32(hdr[0:], 40)
	binary.LittleEndian.PutUint32(hdr[4:], uint32(size))
	binary.LittleEndian.PutUint32(hdr[8:], uint32(size*2)) // picture plus mask
	binary.LittleEndian.PutUint16(hdr[12:], 1)
	binary.LittleEndian.PutUint16(hdr[14:], 32)
	binary.LittleEndian.PutUint32(hdr[20:], uint32(size*size*4+maskRow*size))
	out.Write(hdr)
	for y := size - 1; y >= 0; y-- {
		for x := 0; x < size; x++ {
			c := img.NRGBAAt(x, y)
			out.Write([]byte{c.B, c.G, c.R, c.A})
		}
	}
	out.Write(make([]byte, maskRow*size))
	return out.Bytes()
}

func icoFile(sizes []int, images [][]byte) []byte {
	out := []byte{0, 0, 1, 0, byte(len(sizes)), 0}
	offset := 6 + 16*len(sizes)
	for i, s := range sizes {
		out = append(out, byte(s), byte(s), 0, 0, 1, 0, 32, 0)
		out = binary.LittleEndian.AppendUint32(out, uint32(len(images[i])))
		out = binary.LittleEndian.AppendUint32(out, uint32(offset))
		offset += len(images[i])
	}
	for _, im := range images {
		out = append(out, im...)
	}
	return out
}

// ---------- version details ----------

func utf16z(s string) []byte {
	var b []byte
	for _, u := range append(utf16.Encode([]rune(s)), 0) {
		b = binary.LittleEndian.AppendUint16(b, u)
	}
	return b
}

func pad4(b []byte) []byte {
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

// block builds one node of the version structure: a header (total length,
// value length, type), a name, a value, and child nodes, each part starting
// on a 4-byte boundary.
func block(key string, value []byte, valueLen int, text bool, children ...[]byte) []byte {
	b := make([]byte, 6)
	binary.LittleEndian.PutUint16(b[2:], uint16(valueLen))
	if text {
		b[4] = 1
	}
	b = pad4(append(b, utf16z(key)...))
	b = append(b, value...)
	for _, c := range children {
		b = append(pad4(b), c...)
	}
	binary.LittleEndian.PutUint16(b[0:], uint16(len(b)))
	return b
}

func versionInfo(version, desc, file string) []byte {
	var v [4]uint32
	for i, p := range strings.SplitN(strings.TrimSpace(version), ".", 4) {
		n, _ := strconv.Atoi(p)
		v[i] = uint32(n)
	}
	fixed := make([]byte, 52)
	le := binary.LittleEndian
	le.PutUint32(fixed[0:], 0xFEEF04BD)
	le.PutUint32(fixed[4:], 0x00010000)
	le.PutUint32(fixed[8:], v[0]<<16|v[1])
	le.PutUint32(fixed[12:], v[2]<<16|v[3])
	le.PutUint32(fixed[16:], v[0]<<16|v[1])
	le.PutUint32(fixed[20:], v[2]<<16|v[3])
	le.PutUint32(fixed[24:], 0x3F)
	le.PutUint32(fixed[32:], 0x00040004) // made for Windows NT
	le.PutUint32(fixed[36:], 1)          // an application

	str := func(k, val string) []byte {
		u := utf16z(val)
		return block(k, u, len(u)/2, true)
	}
	table := block("040904B0", nil, 0, true, // US English, Unicode
		str("CompanyName", "Deskbridge"),
		str("FileDescription", desc),
		str("FileVersion", version),
		str("InternalName", strings.TrimSuffix(file, ".exe")),
		str("OriginalFilename", file),
		str("ProductName", "Deskbridge"),
		str("ProductVersion", version),
	)
	return block("VS_VERSION_INFO", fixed, len(fixed), false,
		block("StringFileInfo", nil, 0, true, table),
		block("VarFileInfo", nil, 0, true, block("Translation", []byte{0x09, 0x04, 0xB0, 0x04}, 4, false)),
	)
}

// ---------- the object file ----------

type resource struct {
	typ, id uint16
	data    []byte
}

// coff lays the resources out as Windows expects (a three-level directory:
// type, then id, then language, then the data) inside a one-section COFF
// object. Each data address is stored as an offset within the section with a
// relocation, so the linker can turn it into the final address.
func coff(res []resource, machine, relocType uint16) []byte {
	// res is already ordered by type then id, as the format requires.
	var types []uint16
	byType := map[uint16][]resource{}
	for _, r := range res {
		if _, seen := byType[r.typ]; !seen {
			types = append(types, r.typ)
		}
		byType[r.typ] = append(byType[r.typ], r)
	}
	const dirSize, entrySize, dataEntrySize = 16, 8, 16
	n := len(res)
	// Sizes of the three directory levels, then the data entries.
	level1 := dirSize + entrySize*len(types)
	level2 := dirSize*len(types) + entrySize*n
	level3 := (dirSize + entrySize) * n
	dataEntries := level1 + level2 + level3
	blobs := dataEntries + dataEntrySize*n

	sec := make([]byte, blobs)
	le := binary.LittleEndian
	dir := func(at, count int) { le.PutUint16(sec[at+14:], uint16(count)) }
	entry := func(at int, id uint16, target int, subdir bool) {
		le.PutUint32(sec[at:], uint32(id))
		if subdir {
			target |= 0x80000000
		}
		le.PutUint32(sec[at+4:], uint32(target))
	}
	var relocs []int
	dir(0, len(types))
	l2, l3, de, k := level1, level1+level2, dataEntries, 0
	for i, t := range types {
		entry(dirSize+entrySize*i, t, l2, true)
		list := byType[t]
		dir(l2, len(list))
		for j, r := range list {
			entry(l2+dirSize+entrySize*j, r.id, l3, true)
			dir(l3, 1)
			entry(l3+dirSize, 0x0409, de, false) // language: US English
			for len(sec)%8 != 0 {
				sec = append(sec, 0)
			}
			le.PutUint32(sec[de:], uint32(len(sec))) // offset now, address after linking
			le.PutUint32(sec[de+4:], uint32(len(r.data)))
			relocs = append(relocs, de)
			sec = append(sec, r.data...)
			l3 += dirSize + entrySize
			de += dataEntrySize
			k++
		}
		l2 += dirSize + entrySize*len(list)
	}
	for len(sec)%8 != 0 {
		sec = append(sec, 0)
	}

	const fileHeader, sectionHeader = 20, 40
	out := make([]byte, fileHeader+sectionHeader)
	le.PutUint16(out[0:], machine)
	le.PutUint16(out[2:], 1)                                                        // one section
	le.PutUint32(out[8:], uint32(fileHeader+sectionHeader+len(sec)+10*len(relocs))) // symbol table
	le.PutUint32(out[12:], 1)                                                       // one symbol
	s := out[fileHeader:]
	copy(s, ".rsrc")
	le.PutUint32(s[16:], uint32(len(sec)))
	le.PutUint32(s[20:], fileHeader+sectionHeader)
	le.PutUint32(s[24:], uint32(fileHeader+sectionHeader+len(sec)))
	le.PutUint16(s[32:], uint16(len(relocs)))
	le.PutUint32(s[36:], 0x40000040) // initialised data, readable
	out = append(out, sec...)
	for _, at := range relocs {
		out = le.AppendUint32(out, uint32(at))
		out = le.AppendUint32(out, 0) // symbol 0: the section itself
		out = le.AppendUint16(out, relocType)
	}
	sym := make([]byte, 18)
	copy(sym, ".rsrc")
	le.PutUint16(sym[12:], 1) // section 1
	sym[16] = 3               // static
	out = append(out, sym...)
	return le.AppendUint32(out, 4) // empty string table
}
