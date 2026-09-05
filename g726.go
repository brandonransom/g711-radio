package main

// This file implements a G.726 (formerly G.721) 32kbit/s ADPCM decoder:
// 4-bit codewords in, 16-bit linear PCM samples out. The algorithm (adaptive
// pole-zero predictor + backward-adaptive quantizer scale factor) is the
// CCITT/ITU-T G.721/G.726 reference algorithm, ported to Go from the classic
// Sun Microsystems reference implementation ("g72x.c"/"g721.c"), which Sun
// released with the following notice: "This source code is a product of Sun
// Microsystems, Inc. and is provided for unrestricted use. Users may copy or
// modify this source code without charge." That reference implementation has
// been the basis for G.726 support in numerous open telephony projects for
// decades. Only the 32kbit/s (4-bit codeword) tables/parameters are needed
// here, since that's the only rate these radio streams use.
//
// Byte packing: each octet on the wire carries two 4-bit codewords, with the
// first (earlier in time) sample in the low-order nibble and the second in
// the high-order nibble. This matches common ADPCM/G.726 RTP packetization
// practice. If a decoded stream sounds garbled/noisy rather than like voice
// audio, try swapping the order in decodeG726Frame below.

// dqlntab maps a G.721/G.726-32 codeword to a reconstructed difference
// signal log-magnitude value.
var dqlntab = [16]int16{-2048, 4, 135, 213, 273, 323, 373, 425,
	425, 373, 323, 273, 213, 135, 4, -2048}

// witab maps a codeword to the log of the scale-factor multiplier.
var witab = [16]int16{-12, 18, 41, 64, 112, 198, 355, 1122,
	1122, 355, 198, 112, 64, 41, 18, -12}

// fitab maps a codeword to a value used to track signal stationarity
// (tone/transition detection).
var fitab = [16]int16{0, 0, 0, 0x200, 0x200, 0x200, 0x600, 0xE00,
	0xE00, 0x600, 0x200, 0x200, 0x200, 0, 0, 0}

// power2 is a lookup of powers of two used by quan()/fmult() to find the
// integer part of a base-2 logarithm via linear search (as in the original
// reference code).
var power2 = [15]int16{1, 2, 4, 8, 0x10, 0x20, 0x40, 0x80,
	0x100, 0x200, 0x400, 0x800, 0x1000, 0x2000, 0x4000}

// quan returns i such that table[i-1] <= val < table[i] (linear search, as
// in the reference implementation).
func quan(val int, table []int16, size int) int {
	i := 0
	for ; i < size; i++ {
		if val < int(table[i]) {
			break
		}
	}
	return i
}

// fmult returns the integer product of the 14-bit integer "an" and the
// "floating point" (4-bit exponent, 6-bit mantissa) representation "srn".
func fmult(an, srn int) int {
	var anmag, anexp, anmant int16
	var wanexp, wanmant int16
	var retval int16

	if an > 0 {
		anmag = int16(an)
	} else {
		anmag = int16((-an) & 0x1FFF)
	}
	anexp = int16(quan(int(anmag), power2[:], 15) - 6)
	if anmag == 0 {
		anmant = 32
	} else if anexp >= 0 {
		anmant = anmag >> uint(anexp)
	} else {
		anmant = anmag << uint(-anexp)
	}
	wanexp = anexp + int16((srn>>6)&0xF) - 13

	wanmant = int16((int(anmant)*(srn&077) + 0x30) >> 4)
	if wanexp >= 0 {
		retval = int16((int(wanmant) << uint(wanexp)) & 0x7FFF)
	} else {
		retval = wanmant >> uint(-wanexp)
	}

	if (an ^ srn) < 0 {
		return int(-retval)
	}
	return int(retval)
}

// reconstruct returns the reconstructed difference signal "dq" from a
// codeword's sign and log-magnitude, and the quantizer step size "y".
func reconstruct(sign bool, dqln, y int) int {
	dql := int16(dqln + (y >> 2))
	if dql < 0 {
		if sign {
			return -0x8000
		}
		return 0
	}
	dex := (dql >> 7) & 15
	dqt := 128 + (dql & 127)
	// dex can reach 15, making the shift amount below negative; the
	// original C reference relies on hardware shift-by-negative wrapping
	// to effectively zero the result in that case (this appears to never
	// occur for valid encoder output in practice). Guard explicitly here
	// since Go panics on a negative shift count.
	shift := 14 - dex
	var dq int16
	if shift < 0 {
		dq = 0
	} else {
		dq = (dqt << 7) >> uint(shift)
	}
	if sign {
		return int(dq) - 0x8000
	}
	return int(dq)
}

// g726Decoder holds the per-stream ADPCM decoder state for G.726 (32kbit/s).
// Field names/meanings follow the ITU-T G.721/G.726 recommendation and the
// classic reference implementation, to ease cross-referencing.
type g726Decoder struct {
	yl  int32 // locked/steady-state step size multiplier
	yu  int16 // unlocked/non-steady-state step size multiplier
	dms int16 // short-term energy estimate
	dml int16 // long-term energy estimate
	ap  int16 // linear weighting coefficient of yl and yu

	a  [2]int16 // pole predictor coefficients
	b  [6]int16 // zero predictor coefficients
	pk [2]int16 // signs of the previous two reconstructed samples
	dq [6]int16 // previous 6 quantized difference signal samples (internal float format)
	sr [2]int16 // previous 2 reconstructed signal samples (internal float format)
	td bool     // delayed tone/transition detect flag
}

// newG726Decoder returns a decoder initialized to the reference algorithm's
// specified reset state.
func newG726Decoder() *g726Decoder {
	d := &g726Decoder{
		yl: 34816,
		yu: 544,
	}
	for i := range d.sr {
		d.sr[i] = 32
	}
	for i := range d.dq {
		d.dq[i] = 32
	}
	return d
}

func (d *g726Decoder) predictorZero() int {
	sezi := fmult(int(d.b[0])>>2, int(d.dq[0]))
	for i := 1; i < 6; i++ {
		sezi += fmult(int(d.b[i])>>2, int(d.dq[i]))
	}
	return sezi
}

func (d *g726Decoder) predictorPole() int {
	return fmult(int(d.a[1])>>2, int(d.sr[1])) + fmult(int(d.a[0])>>2, int(d.sr[0]))
}

func (d *g726Decoder) stepSize() int {
	if d.ap >= 256 {
		return int(d.yu)
	}
	y := int(d.yl >> 6)
	dif := int(d.yu) - y
	al := int(d.ap) >> 2
	if dif > 0 {
		y += (dif * al) >> 6
	} else if dif < 0 {
		y += (dif*al + 0x3F) >> 6
	}
	return y
}

// update updates the decoder state for one output codeword, following the
// reference algorithm's update() routine. code_size is fixed at 4 for
// G.726-32.
func (d *g726Decoder) update(y, wi, fi, dq, sr, dqsez int) {
	var pk0 int16
	if dqsez < 0 {
		pk0 = 1
	}

	mag := int16(dq & 0x7FFF)

	ylint := int16(d.yl >> 15)
	ylfrac := int16((d.yl >> 10) & 0x1F)
	thr1 := (32 + ylfrac) << uint(ylint)
	var thr2 int16
	if ylint > 9 {
		thr2 = 31 << 10
	} else {
		thr2 = thr1
	}
	dqthr := (thr2 + (thr2 >> 1)) >> 1

	var tr bool
	if !d.td {
		tr = false
	} else if mag <= dqthr {
		tr = false
	} else {
		tr = true
	}

	// Quantizer scale factor adaptation.
	d.yu = int16(y + ((wi - y) >> 5))
	if d.yu < 544 {
		d.yu = 544
	} else if d.yu > 5120 {
		d.yu = 5120
	}
	d.yl += int32(d.yu) + ((-d.yl) >> 6)

	// Adaptive predictor coefficients.
	var a2p int16
	if tr {
		d.a[0] = 0
		d.a[1] = 0
		for i := range d.b {
			d.b[i] = 0
		}
	} else {
		pks1 := pk0 ^ d.pk[0]

		a2p = d.a[1] - (d.a[1] >> 7)
		if dqsez != 0 {
			var fa1 int16
			if pks1 != 0 {
				fa1 = d.a[0]
			} else {
				fa1 = -d.a[0]
			}
			if fa1 < -8191 {
				a2p -= 0x100
			} else if fa1 > 8191 {
				a2p += 0xFF
			} else {
				a2p += fa1 >> 5
			}

			if (pk0 ^ d.pk[1]) != 0 {
				if a2p <= -12160 {
					a2p = -12288
				} else if a2p >= 12416 {
					a2p = 12288
				} else {
					a2p -= 0x80
				}
			} else if a2p <= -12416 {
				a2p = -12288
			} else if a2p >= 12160 {
				a2p = 12288
			} else {
				a2p += 0x80
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

		a1ul := int16(15360) - a2p
		if d.a[0] < -a1ul {
			d.a[0] = -a1ul
		} else if d.a[0] > a1ul {
			d.a[0] = a1ul
		}

		for cnt := range d.b {
			// code_size is always 4 here (G.726-32), matching the
			// reference's "for G.721 and 24Kbps G.723" branch.
			d.b[cnt] -= d.b[cnt] >> 8
			if dq&0x7FFF != 0 {
				if (int16(dq) ^ d.dq[cnt]) >= 0 {
					d.b[cnt] += 128
				} else {
					d.b[cnt] -= 128
				}
			}
		}
	}

	for cnt := 5; cnt > 0; cnt-- {
		d.dq[cnt] = d.dq[cnt-1]
	}
	if mag == 0 {
		if dq >= 0 {
			d.dq[0] = 0x20
		} else {
			d.dq[0] = -992 // 0xFC20 as a two's-complement int16
		}
	} else {
		exp := int16(quan(int(mag), power2[:], 15))
		base := (exp << 6) + int16((int(mag)<<6)>>uint(exp))
		if dq >= 0 {
			d.dq[0] = base
		} else {
			d.dq[0] = base - 0x400
		}
	}

	d.sr[1] = d.sr[0]
	if sr == 0 {
		d.sr[0] = 0x20
	} else if sr > 0 {
		exp := int16(quan(sr, power2[:], 15))
		d.sr[0] = (exp << 6) + int16((sr<<6)>>uint(exp))
	} else if sr > -32768 {
		magS := int16(-sr)
		exp := int16(quan(int(magS), power2[:], 15))
		d.sr[0] = (exp << 6) + int16((int(magS)<<6)>>uint(exp)) - 0x400
	} else {
		d.sr[0] = -992 // 0xFC20 as a two's-complement int16
	}

	d.pk[1] = d.pk[0]
	d.pk[0] = pk0

	if tr {
		d.td = false
	} else if a2p < -11776 {
		d.td = true
	} else {
		d.td = false
	}

	// Adaptation speed control.
	d.dms += (int16(fi) - d.dms) >> 5
	d.dml += int16((int32(fi)<<2 - int32(d.dml)) >> 7)

	if tr {
		d.ap = 256
	} else if y < 1536 {
		d.ap += (0x200 - d.ap) >> 4
	} else if d.td {
		d.ap += (0x200 - d.ap) >> 4
	} else {
		diff := int(d.dms)<<2 - int(d.dml)
		if diff < 0 {
			diff = -diff
		}
		if diff >= int(d.dml)>>3 {
			d.ap += (0x200 - d.ap) >> 4
		} else {
			d.ap += (-d.ap) >> 4
		}
	}
}

// decodeSample decodes one 4-bit codeword to a 16-bit linear PCM sample.
func (d *g726Decoder) decodeSample(code int) int16 {
	i := code & 0x0f

	sezi := int16(d.predictorZero())
	sez := sezi >> 1
	sei := sezi + int16(d.predictorPole())
	se := sei >> 1

	y := int16(d.stepSize())

	dq := int16(reconstruct(i&0x08 != 0, int(dqlntab[i]), int(y)))

	var sr int16
	if dq < 0 {
		sr = se - (dq & 0x3FFF)
	} else {
		sr = se + dq
	}

	dqsez := sr - se + sez

	d.update(int(y), int(witab[i])<<5, int(fitab[i]), int(dq), int(sr), int(dqsez))

	// se/sr represent a 14-bit dynamic range signal; shift to the full
	// 16-bit range, matching the reference decoder's AUDIO_ENCODING_LINEAR
	// output path.
	return sr << 2
}

// decodeG726Frame decodes a G.726-32 wire frame (2 packed 4-bit codewords per
// byte) into linear PCM samples, advancing the decoder's persistent state.
func (d *g726Decoder) decodeG726Frame(codes []byte) []int16 {
	pcm := make([]int16, len(codes)*2)
	for i, b := range codes {
		lo := int(b & 0x0F)
		hi := int(b >> 4)
		pcm[i*2] = d.decodeSample(lo)
		pcm[i*2+1] = d.decodeSample(hi)
	}
	return pcm
}
