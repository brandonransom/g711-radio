package main

// This file implements a decoder for the proprietary "Telex 32k" vocoder
// used by some radio hardware on this network, reverse-engineered from wire
// captures (no public specification exists for this format). Empirically
// determined structure:
//
//   - 32kbit/s, 2 samples packed per wire byte (4 bits/sample), giving an
//     8kHz output sample rate — the same sample count per 20ms packet as
//     the G.711 frames on this network (160 samples), so no resampling is
//     needed anywhere downstream.
//   - Each 4-bit codeword is sign+magnitude: the top bit is sign, the
//     bottom 3 bits are a magnitude level 0-7. This was confirmed by nibble
//     statistics on real captures: (magnitude, magnitude+8) pairs have
//     near-identical probabilities (a sign bit), and probability decreases
//     monotonically with magnitude (the classic differential-quantizer
//     codebook shape). Magnitude 0 specifically means "no change" (a true
//     zero delta) — this was confirmed against a capture with known silence
//     on the input, which showed only codes 0/1/9 (i.e. hold, and small
//     +/-1 dither), matching classic ADPCM idle-channel "hunting" behavior.
//   - The decoder is a leaky accumulator (as opposed to a bare running sum,
//     which was found to drift unboundedly on longer streams) driven by a
//     per-magnitude-code step size, itself scaled by an adaptive factor
//     that grows on a run of large-magnitude codes and shrinks otherwise
//     (the standard "Jayant"-style companding adaptation found in most
//     ADPCM-family codecs) — this let real speech reach usable volume
//     without either flattening quiet passages or clipping loud ones.
//
// What's NOT captured here: real captures of a steady tone (both a locally
// generated test tone and a signal generator feeding the actual encoder
// input, confirmed via the device's own level meter) still decode with a
// meaningfully unstable/noisy envelope, even though the underlying wire
// data's sign pattern is a clean, correctly-periodic tone. Smoothing this
// decoder's scale adaptation (tested separately) did not change that
// instability at all, which points to it being a real characteristic of
// the source hardware's own encoder rather than something fixable here.
// Voice intelligibility is solid; a persistent "noise curtain" texture on
// top of speech is a known, currently-unresolved limitation.
var telexScaleTable = [8]float64{0.85, 0.9, 0.95, 1.0, 1.05, 1.15, 1.3, 1.5}

const (
	telexStepBase = 10.0
	telexStepMult = 1.3
	telexLeak     = 0.999 // leaky-integrator decay per sample; see note above on drift
	telexScaleMin = 1.0
	telexScaleMax = 50.0

	// defaultTelexOutputGain is used unless overridden by config.json's
	// "telexOutputGain" field (see loadConfig). It's a fixed linear gain
	// from raw accumulator units to int16 PCM, picked conservatively to
	// leave headroom against clipping on louder-than-typical transmissions;
	// tune it upward if audio is consistently too quiet.
	defaultTelexOutputGain = 24.0
)

// telexOutputGain is set from config at startup (see loadConfig); it's a
// package-level var (like the codebase's other tunable decoder knobs) so it
// can be adjusted without a rebuild.
var telexOutputGain = defaultTelexOutputGain

// telexDecoder holds the per-stream decoder state for the Telex 32k codec.
type telexDecoder struct {
	accum float64
	scale float64
}

func newTelexDecoder() *telexDecoder {
	return &telexDecoder{scale: 1.0}
}

// decodeNibble decodes one 4-bit sign+magnitude codeword to a linear PCM
// sample (pre-gain, in raw accumulator units).
func (d *telexDecoder) decodeNibble(n byte) float64 {
	sign := (n >> 3) & 1
	mag := n & 0x7

	var delta float64
	if mag > 0 {
		delta = d.scale * telexStepBase * telexPow(telexStepMult, float64(mag-1))
	}
	if sign != 0 {
		delta = -delta
	}
	d.accum = d.accum*telexLeak + delta

	d.scale *= telexScaleTable[mag]
	if d.scale < telexScaleMin {
		d.scale = telexScaleMin
	} else if d.scale > telexScaleMax {
		d.scale = telexScaleMax
	}
	return d.accum
}

// telexPow is a tiny fixed-base exponent helper so this file doesn't need to
// import math just for math.Pow with an integer exponent.
func telexPow(base float64, exp float64) float64 {
	result := 1.0
	for i := 0; i < int(exp); i++ {
		result *= base
	}
	return result
}

// decodeTelexFrame decodes a Telex 32k wire frame (2 packed 4-bit codewords
// per byte) into 16-bit linear PCM samples, advancing the decoder's
// persistent state. Output is clamped to the valid int16 range as a safety
// net against any single frame producing an out-of-range value.
func (d *telexDecoder) decodeTelexFrame(codes []byte) []int16 {
	pcm := make([]int16, len(codes)*2)
	for i, b := range codes {
		hi := d.decodeNibble(b>>4) * telexOutputGain
		lo := d.decodeNibble(b&0x0F) * telexOutputGain
		pcm[i*2] = clampInt16(hi)
		pcm[i*2+1] = clampInt16(lo)
	}
	return pcm
}

func clampInt16(v float64) int16 {
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return -32768
	}
	return int16(v)
}
