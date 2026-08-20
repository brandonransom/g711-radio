package main

import (
	"math"
	"time"
)

const (
	// pcmuToLinear maps G.711 µ-law byte values to 16-bit linear PCM.
	// Calculated from the standard µ-law expansion formula.
	pcmuBias = 33

	vadSampleRate    = 8000
	vadFrameBytes    = 160 // 20ms at 8kHz
	vadFrameDuration = 20 * time.Millisecond
)

// pcmuTable is a precomputed lookup for G.711 µ-law → int16 PCM.
var pcmuTable [256]int16

func init() {
	for i := 0; i < 256; i++ {
		b := ^byte(i)
		sign := b & 0x80
		exp := (b >> 4) & 0x07
		mantissa := b & 0x0F
		sample := int16((int(mantissa)<<1 | 1) << (int(exp) + 2))
		sample += pcmuBias
		if sign == 0 {
			sample = -sample
		}
		pcmuTable[i] = sample
	}
}

// DecodePCMU converts a G.711 µ-law frame to int16 PCM samples.
func DecodePCMU(pcmu []byte) []int16 {
	out := make([]int16, len(pcmu))
	for i, b := range pcmu {
		out[i] = pcmuTable[b]
	}
	return out
}

// rmsEnergy returns the root-mean-square energy of a PCM frame,
// normalised to [0.0, 1.0] relative to int16 max.
func rmsEnergy(samples []int16) float64 {
	if len(samples) == 0 {
		return 0
	}
	var sum float64
	for _, s := range samples {
		v := float64(s) / 32768.0
		sum += v * v
	}
	return math.Sqrt(sum / float64(len(samples)))
}

// vadState tracks voice activity for a single station.
type vadState struct {
	threshold    float64       // RMS energy level that triggers speech
	silenceGrace time.Duration // how long silence must hold before clip is finalised
	minClip      time.Duration // discard clips shorter than this (squelch noise)
	maxClip      time.Duration // hard cap on clip length

	speaking      bool
	silenceStart  time.Time
	clipBuf       []int16
	clipStart     time.Time
	onClip        func(samples []int16, start time.Time)
}

func newVADState(threshold float64, silenceGrace, minClip, maxClip time.Duration, onClip func([]int16, time.Time)) *vadState {
	return &vadState{
		threshold:    threshold,
		silenceGrace: silenceGrace,
		minClip:      minClip,
		maxClip:      maxClip,
		onClip:       onClip,
	}
}

// Push processes one decoded PCM frame and fires onClip when a transmission ends.
func (v *vadState) Push(samples []int16, now time.Time) {
	energy := rmsEnergy(samples)

	if energy >= v.threshold {
		if !v.speaking {
			v.speaking = true
			v.clipStart = now
			v.clipBuf = v.clipBuf[:0]
		}
		v.silenceStart = time.Time{} // reset silence timer
		v.clipBuf = append(v.clipBuf, samples...)

		// Hard cap: finalise early if clip is getting too long.
		clipDur := time.Duration(len(v.clipBuf)) * time.Second / vadSampleRate
		if clipDur >= v.maxClip {
			v.finalise()
		}
		return
	}

	// Silence frame.
	if !v.speaking {
		return
	}

	if v.silenceStart.IsZero() {
		v.silenceStart = now
	}
	v.clipBuf = append(v.clipBuf, samples...)

	if now.Sub(v.silenceStart) >= v.silenceGrace {
		v.finalise()
	}
}

func (v *vadState) finalise() {
	samples := make([]int16, len(v.clipBuf))
	copy(samples, v.clipBuf)
	start := v.clipStart

	v.speaking = false
	v.silenceStart = time.Time{}
	v.clipBuf = v.clipBuf[:0]

	clipDur := time.Duration(len(samples)) * time.Second / vadSampleRate
	if clipDur < v.minClip {
		return // too short — discard
	}

	v.onClip(samples, start)
}
