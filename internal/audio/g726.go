package audio

import "fmt"

// G.726 ADPCM decoder for 16, 24, 32 and 40 kbit/s, following the ITU-T
// G.726 block structure (the same algorithm as the public-domain CCITT
// reference implementation).

type g726Tables struct {
	bits   uint
	dqln   []int16
	wi     []int16
	fi     []int16
	wShift uint
	mask   int
}

var g726Rates = map[int]*g726Tables{
	16000: {
		bits: 2,
		dqln: []int16{116, 365, 365, 116},
		wi:   []int16{-704, 14048, 14048, -704},
		fi:   []int16{0, 0xE00, 0xE00, 0},
		mask: 0x3FFF,
	},
	24000: {
		bits: 3,
		dqln: []int16{-2048, 135, 273, 373, 373, 273, 135, -2048},
		wi:   []int16{-128, 960, 4384, 18624, 18624, 4384, 960, -128},
		fi:   []int16{0, 0x200, 0x400, 0xE00, 0xE00, 0x400, 0x200, 0},
		mask: 0x3FFF,
	},
	32000: {
		bits:   4,
		dqln:   []int16{-2048, 4, 135, 213, 273, 323, 373, 425, 425, 373, 323, 273, 213, 135, 4, -2048},
		wi:     []int16{-12, 18, 41, 64, 112, 198, 355, 1122, 1122, 355, 198, 112, 64, 41, 18, -12},
		fi:     []int16{0, 0, 0, 0x200, 0x200, 0x200, 0x600, 0xE00, 0xE00, 0x600, 0x200, 0x200, 0x200, 0, 0, 0},
		wShift: 5,
		mask:   0x3FFF,
	},
	40000: {
		bits: 5,
		dqln: []int16{-2048, -66, 28, 104, 169, 224, 274, 318, 358, 395, 429, 459, 488, 514, 539, 566,
			566, 539, 514, 488, 459, 429, 395, 358, 318, 274, 224, 169, 104, 28, -66, -2048},
		wi: []int16{448, 448, 768, 1248, 1280, 1312, 1856, 3200, 4512, 5728, 7008, 8960, 11456, 14080, 16928, 22272,
			22272, 16928, 14080, 11456, 8960, 7008, 5728, 4512, 3200, 1856, 1312, 1280, 1248, 768, 448, 448},
		fi: []int16{0, 0, 0, 0, 0, 0x200, 0x200, 0x200, 0x200, 0x200, 0x400, 0x600, 0x800, 0xA00, 0xC00, 0xC00,
			0xC00, 0xC00, 0xA00, 0x800, 0x600, 0x400, 0x200, 0x200, 0x200, 0x200, 0x200, 0, 0, 0, 0, 0},
		mask: 0x7FFF,
	},
}

var g726Power2 = [15]int{1, 2, 4, 8, 0x10, 0x20, 0x40, 0x80, 0x100, 0x200, 0x400, 0x800, 0x1000, 0x2000, 0x4000}

func g726Quan(val int) int {
	for i, p := range g726Power2 {
		if val < p {
			return i
		}
	}
	return len(g726Power2)
}

func g726Fmult(an, srn int) int {
	anmag := an
	if an <= 0 {
		anmag = (-an) & 0x1FFF
	}
	anexp := g726Quan(anmag) - 6
	var anmant int
	switch {
	case anmag == 0:
		anmant = 32
	case anexp >= 0:
		anmant = anmag >> uint(anexp)
	default:
		anmant = anmag << uint(-anexp)
	}
	wanexp := anexp + ((srn >> 6) & 0xF) - 13
	wanmant := (anmant*(srn&0x3F) + 0x30) >> 4
	var ret int
	if wanexp >= 0 {
		ret = (wanmant << uint(wanexp)) & 0x7FFF
	} else {
		ret = wanmant >> uint(-wanexp)
	}
	if (an ^ srn) < 0 {
		return -ret
	}
	return ret
}

// G726Decoder decodes a G.726 stream to 16-bit PCM.
type G726Decoder struct {
	t         *g726Tables
	bigEndian bool

	yl  int
	yu  int
	dms int
	dml int
	ap  int
	a   [2]int
	b   [6]int
	pk  [2]int
	dq  [6]int
	sr  [2]int
	td  int

	acc   uint32 // bit accumulator
	nbits uint

	// leak40 is the zero-predictor leak shift at 40 kbit/s. ITU-T G.726
	// specifies 2^-9 there (2^-8 at the other rates). Tests lower it to 8 to
	// compare against FFmpeg, whose codec uses 2^-8 at every rate.
	leak40 uint
}

// NewG726Decoder returns a decoder for the given bit rate (16000, 24000,
// 32000 or 40000). bigEndian selects the AAL2 packing (codes packed starting
// at the most significant bit); otherwise the RFC 3551 packing is used.
func NewG726Decoder(bitRate int, bigEndian bool) (*G726Decoder, error) {
	t, ok := g726Rates[bitRate]
	if !ok {
		return nil, fmt.Errorf("unsupported G.726 bit rate %d", bitRate)
	}
	d := &G726Decoder{t: t, bigEndian: bigEndian, leak40: 9}
	d.Reset()
	return d, nil
}

// Reset returns the decoder to its initial state.
func (d *G726Decoder) Reset() {
	d.yl, d.yu = 34816, 544
	d.dms, d.dml, d.ap, d.td = 0, 0, 0, 0
	d.a = [2]int{}
	d.pk = [2]int{}
	d.sr = [2]int{32, 32}
	d.b = [6]int{}
	d.dq = [6]int{32, 32, 32, 32, 32, 32}
	d.acc, d.nbits = 0, 0
}

// Decode appends the PCM decoding of in to dst.
func (d *G726Decoder) Decode(dst []int16, in []byte) []int16 {
	bits := d.t.bits
	for _, b := range in {
		if d.bigEndian {
			d.acc = d.acc<<8 | uint32(b)
			d.nbits += 8
			for d.nbits >= bits {
				code := int(d.acc>>(d.nbits-bits)) & (1<<bits - 1)
				d.nbits -= bits
				dst = append(dst, d.decodeCode(code))
			}
			d.acc &= 1<<d.nbits - 1
		} else {
			d.acc |= uint32(b) << d.nbits
			d.nbits += 8
			for d.nbits >= bits {
				code := int(d.acc) & (1<<bits - 1)
				d.acc >>= bits
				d.nbits -= bits
				dst = append(dst, d.decodeCode(code))
			}
		}
	}
	return dst
}

func (d *G726Decoder) predictorZero() int {
	sezi := 0
	for i := 0; i < 6; i++ {
		sezi += g726Fmult(d.b[i]>>2, d.dq[i])
	}
	return sezi
}

func (d *G726Decoder) predictorPole() int {
	return g726Fmult(d.a[1]>>2, d.sr[1]) + g726Fmult(d.a[0]>>2, d.sr[0])
}

func (d *G726Decoder) stepSize() int {
	if d.ap >= 256 {
		return d.yu
	}
	y := d.yl >> 6
	dif := d.yu - y
	al := d.ap >> 2
	if dif > 0 {
		y += (dif * al) >> 6
	} else if dif < 0 {
		y += (dif*al + 0x3F) >> 6
	}
	return y
}

func g726Reconstruct(sign bool, dqln, y int) int {
	dql := dqln + (y >> 2)
	if dql < 0 {
		if sign {
			return -0x8000
		}
		return 0
	}
	dex := (dql >> 7) & 15
	dqt := 128 + (dql & 127)
	dq := int(int16((dqt << 7) >> uint(14-dex)))
	if sign {
		return dq - 0x8000
	}
	return dq
}

func (d *G726Decoder) decodeCode(code int) int16 {
	t := d.t
	sezi := d.predictorZero()
	sez := sezi >> 1
	sei := sezi + d.predictorPole()
	se := sei >> 1

	y := d.stepSize()
	signBit := 1 << (t.bits - 1)
	dq := g726Reconstruct(code&signBit != 0, int(t.dqln[code]), y)

	var sr int
	if dq < 0 {
		sr = se - (dq & t.mask)
	} else {
		sr = se + dq
	}
	sr = int(int16(sr))
	dqsez := int(int16(sr - se + sez))

	d.update(int(t.bits), y, int(t.wi[code])<<t.wShift, int(t.fi[code]), dq, sr, dqsez)

	out := sr << 2
	if out > 32767 {
		out = 32767
	} else if out < -32768 {
		out = -32768
	}
	return int16(out)
}

func (d *G726Decoder) update(codeSize, y, wi, fi, dq, sr, dqsez int) {
	pk0 := 0
	if dqsez < 0 {
		pk0 = 1
	}
	mag := dq & 0x7FFF

	// TRANS
	ylint := d.yl >> 15
	ylfrac := (d.yl >> 10) & 0x1F
	thr1 := (32 + ylfrac) << uint(ylint)
	thr2 := thr1
	if ylint > 9 {
		thr2 = 31 << 10
	}
	dqthr := (thr2 + (thr2 >> 1)) >> 1
	tr := 0
	if d.td != 0 && mag > dqthr {
		tr = 1
	}

	// Quantizer scale factor adaptation.
	d.yu = y + ((wi - y) >> 5)
	if d.yu < 544 {
		d.yu = 544
	} else if d.yu > 5120 {
		d.yu = 5120
	}
	d.yl += d.yu + ((-d.yl) >> 6)

	// Adaptive predictor coefficients.
	a2p := 0
	if tr == 1 {
		d.a = [2]int{}
		d.b = [6]int{}
	} else {
		pks1 := pk0 ^ d.pk[0]

		a2p = d.a[1] - (d.a[1] >> 7)
		if dqsez != 0 {
			fa1 := -d.a[0]
			if pks1 != 0 {
				fa1 = d.a[0]
			}
			switch {
			case fa1 < -8191:
				a2p -= 0x100
			case fa1 > 8191:
				a2p += 0xFF
			default:
				a2p += fa1 >> 5
			}
			if pk0^d.pk[1] != 0 {
				switch {
				case a2p <= -12160:
					a2p = -12288
				case a2p >= 12416:
					a2p = 12288
				default:
					a2p -= 0x80
				}
			} else {
				switch {
				case a2p <= -12416:
					a2p = -12288
				case a2p >= 12160:
					a2p = 12288
				default:
					a2p += 0x80
				}
			}
		}
		d.a[1] = a2p

		d.a[0] -= d.a[0] >> 8
		if dqsez != 0 {
			if pks1 == 0 {
				d.a[0] += 192
			} else {
				d.a[0] -= 192
			}
		}
		a1ul := 15360 - a2p
		if d.a[0] < -a1ul {
			d.a[0] = -a1ul
		} else if d.a[0] > a1ul {
			d.a[0] = a1ul
		}

		for i := 0; i < 6; i++ {
			if codeSize == 5 {
				d.b[i] -= d.b[i] >> d.leak40
			} else {
				d.b[i] -= d.b[i] >> 8
			}
			if dq&0x7FFF != 0 {
				if (dq ^ d.dq[i]) >= 0 {
					d.b[i] += 128
				} else {
					d.b[i] -= 128
				}
			}
		}
	}

	for i := 5; i > 0; i-- {
		d.dq[i] = d.dq[i-1]
	}
	// FLOAT A
	if mag == 0 {
		if dq >= 0 {
			d.dq[0] = 0x20
		} else {
			d.dq[0] = int(int16(-992)) // 0xFC20
		}
	} else {
		exp := g726Quan(mag)
		v := (exp << 6) + ((mag << 6) >> uint(exp))
		if dq < 0 {
			v -= 0x400
		}
		d.dq[0] = v
	}

	d.sr[1] = d.sr[0]
	// FLOAT B
	switch {
	case sr == 0:
		d.sr[0] = 0x20
	case sr > 0:
		exp := g726Quan(sr)
		d.sr[0] = (exp << 6) + ((sr << 6) >> uint(exp))
	case sr > -32768:
		m := -sr
		exp := g726Quan(m)
		d.sr[0] = (exp << 6) + ((m << 6) >> uint(exp)) - 0x400
	default:
		d.sr[0] = int(int16(-992))
	}

	d.pk[1] = d.pk[0]
	d.pk[0] = pk0

	// TONE
	switch {
	case tr == 1:
		d.td = 0
	case a2p < -11776:
		d.td = 1
	default:
		d.td = 0
	}

	// Adaptive speed control.
	d.dms += (fi - d.dms) >> 5
	d.dml += ((fi << 2) - d.dml) >> 7

	diff := (d.dms << 2) - d.dml
	if diff < 0 {
		diff = -diff
	}
	switch {
	case tr == 1:
		d.ap = 256
	case y < 1536, d.td == 1, diff >= (d.dml >> 3):
		d.ap += (0x200 - d.ap) >> 4
	default:
		d.ap += (-d.ap) >> 4
	}
}
