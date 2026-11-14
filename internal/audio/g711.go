// Package audio converts telephone-grade camera audio (G.711 µ-law, G.711
// A-law, G.726) into AAC-LC, the only transcoding ButterCams performs
// (spec 6.3). Everything here is pure Go.
package audio

var (
	mulawTable [256]int16
	alawTable  [256]int16
)

func init() {
	for i := 0; i < 256; i++ {
		// G.711 µ-law
		u := ^byte(i)
		t := (int(u&0x0F) << 3) + 0x84
		t <<= (u & 0x70) >> 4
		if u&0x80 != 0 {
			mulawTable[i] = int16(0x84 - t)
		} else {
			mulawTable[i] = int16(t - 0x84)
		}

		// G.711 A-law
		a := byte(i) ^ 0x55
		seg := (a & 0x70) >> 4
		v := int(a&0x0F) << 4
		switch seg {
		case 0:
			v += 8
		default:
			v = (v + 0x108) << (seg - 1)
		}
		if a&0x80 != 0 {
			alawTable[i] = int16(v)
		} else {
			alawTable[i] = int16(-v)
		}
	}
}

// DecodeMulaw appends the 16-bit PCM decoding of G.711 µ-law bytes to dst.
func DecodeMulaw(dst []int16, in []byte) []int16 {
	for _, b := range in {
		dst = append(dst, mulawTable[b])
	}
	return dst
}

// DecodeAlaw appends the 16-bit PCM decoding of G.711 A-law bytes to dst.
func DecodeAlaw(dst []int16, in []byte) []int16 {
	for _, b := range in {
		dst = append(dst, alawTable[b])
	}
	return dst
}
