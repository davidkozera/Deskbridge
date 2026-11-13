package proto

import (
	"encoding/binary"
	"math"
)

// ToStereo16 converts interleaved PCM (16-bit int, or 32-bit float/int) with
// any channel count into 16-bit stereo, appending to dst.
func ToStereo16(dst, src []byte, frames, channels, bits int, float bool) []byte {
	bps := bits / 8
	sample := func(f, c int) int16 {
		if c >= channels {
			c = 0
		}
		o := (f*channels + c) * bps
		switch {
		case float && bits == 32:
			v := math.Float32frombits(binary.LittleEndian.Uint32(src[o:]))
			if v > 1 {
				v = 1
			} else if v < -1 {
				v = -1
			}
			return int16(v * 32767)
		case bits == 16:
			return int16(binary.LittleEndian.Uint16(src[o:]))
		case bits == 24:
			return int16(uint16(src[o+1]) | uint16(src[o+2])<<8)
		case bits == 32:
			return int16(binary.LittleEndian.Uint32(src[o:]) >> 16)
		}
		return 0
	}
	for f := 0; f < frames; f++ {
		dst = binary.LittleEndian.AppendUint16(dst, uint16(sample(f, 0)))
		dst = binary.LittleEndian.AppendUint16(dst, uint16(sample(f, 1)))
	}
	return dst
}
