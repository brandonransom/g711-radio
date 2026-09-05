package main

import (
	"math"
	"testing"
)

// qtab721 is the G.721/G.726-32 quantizer table, only needed to build a
// matching encoder for this round-trip self-test (the production code only
// needs a decoder).
var qtab721 = [7]int16{-124, 80, 178, 246, 300, 349, 400}

// quantizeTest mirrors the reference algorithm's quantize(): given a raw
// difference-signal sample and step size, returns the ADPCM codeword.
func quantizeTest(dVal, y int, table []int16, size int) int {
	dqm := dVal
	if dqm < 0 {
		dqm = -dqm
	}
	exp := int16(quan(dqm>>1, power2[:], 15))
	mant := int16(((dqm << 7) >> uint(exp)) & 0x7F)
	dl := (exp << 7) + mant

	dln := int(dl) - (y >> 2)

	i := quan(dln, table, size)
	if dVal < 0 {
		return (size << 1) + 1 - i
	} else if i == 0 {
		return (size << 1) + 1
	}
	return i
}

// encodeSample mirrors g721_encoder(): encodes one 14-bit-range linear PCM
// sample (already >>2 from 16-bit) to a 4-bit G.726 codeword, advancing the
// same decoder state struct used for decoding (encode/decode share identical
// predictor/quantizer-adaptation logic in this algorithm).
func (d *g726Decoder) encodeSample(sl int) int {
	sezi := d.predictorZero()
	sez := sezi >> 1
	se := (sezi + d.predictorPole()) >> 1

	dVal := sl - se

	y := d.stepSize()
	i := quantizeTest(dVal, y, qtab721[:], 7)

	dq := reconstruct(i&8 != 0, int(dqlntab[i]), y)

	var sr int
	if dq < 0 {
		sr = se - (dq & 0x3FFF)
	} else {
		sr = se + dq
	}

	dqsez := sr + sez - se

	d.update(y, int(witab[i])<<5, int(fitab[i]), dq, sr, dqsez)

	return i
}

// encodeG726Frame encodes 16-bit linear PCM samples to a G.726-32 wire frame
// (2 codewords packed per byte, matching decodeG726Frame's nibble order).
func encodeG726Frame(enc *g726Decoder, pcm []int16) []byte {
	codes := make([]byte, len(pcm)/2)
	for i := 0; i < len(codes); i++ {
		lo := enc.encodeSample(int(pcm[i*2]) >> 2)
		hi := enc.encodeSample(int(pcm[i*2+1]) >> 2)
		codes[i] = byte(lo&0x0F) | byte(hi&0x0F)<<4
	}
	return codes
}

// TestG726RoundTrip encodes a synthetic tone with a from-scratch encoder
// (built from the same reference algorithm as the production decoder) and
// confirms the production decoder reconstructs a waveform that closely
// tracks the original — the best available correctness signal without
// official ITU-T test vectors on hand.
func TestG726RoundTrip(t *testing.T) {
	const (
		sampleRate = 8000
		freqHz     = 440.0
		numSamples = 800 // 100ms
		amplitude  = 8000
	)

	original := make([]int16, numSamples)
	for i := range original {
		original[i] = int16(amplitude * math.Sin(2*math.Pi*freqHz*float64(i)/sampleRate))
	}

	enc := newG726Decoder()
	codes := encodeG726Frame(enc, original)
	if len(codes) != numSamples/2 {
		t.Fatalf("expected %d encoded bytes, got %d", numSamples/2, len(codes))
	}

	dec := newG726Decoder()
	decoded := dec.decodeG726Frame(codes)
	if len(decoded) != numSamples {
		t.Fatalf("expected %d decoded samples, got %d", numSamples, len(decoded))
	}

	// Compare RMS error against signal RMS — G.726 at 32kbit/s is lossy, so
	// exact equality isn't expected, but the reconstruction should closely
	// track the original tone (low relative error), skipping the first few
	// samples while predictor/quantizer state converges from its reset
	// values.
	const warmup = 32
	var sigEnergy, errEnergy float64
	for i := warmup; i < numSamples; i++ {
		diff := float64(decoded[i]) - float64(original[i])
		errEnergy += diff * diff
		sigEnergy += float64(original[i]) * float64(original[i])
	}
	relErr := math.Sqrt(errEnergy / sigEnergy)
	if relErr > 0.15 {
		t.Fatalf("G.726 round-trip relative error too high: %.4f (decoded doesn't track original waveform)", relErr)
	}
	t.Logf("G.726 round-trip relative RMS error: %.4f", relErr)
}

// TestEncodePCMURoundTrip confirms EncodePCMU exactly inverts DecodePCMU for
// every one of the 256 µ-law byte values.
func TestEncodePCMURoundTrip(t *testing.T) {
	for i := 0; i < 256; i++ {
		b := byte(i)
		sample := pcmuTable[b]
		got := EncodePCMU(sample)
		if got != b {
			t.Errorf("EncodePCMU(DecodePCMU(%d)) = %d, want %d", b, got, b)
		}
	}
}
