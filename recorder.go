package main

import (
	"sort"
	"sync"
	"time"
)

const (
	// pcmuBias maps G.711 µ-law byte values to 16-bit linear PCM.
	// Calculated from the standard µ-law expansion formula.
	pcmuBias = 33

	recSampleRate = 8000
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

// mulawLevels/mulawLevelBytes are the 256 representable µ-law PCM levels,
// sorted ascending, paired with the byte that decodes to each. Built from
// pcmuTable (rather than a separately implemented encode algorithm) so that
// EncodePCMU is guaranteed to be the exact inverse of DecodePCMU.
var mulawLevels [256]int16
var mulawLevelBytes [256]byte

func init() {
	type pair struct {
		level int16
		b     byte
	}
	pairs := make([]pair, 256)
	for i := 0; i < 256; i++ {
		pairs[i] = pair{pcmuTable[i], byte(i)}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].level < pairs[j].level })
	for i, p := range pairs {
		mulawLevels[i] = p.level
		mulawLevelBytes[i] = p.b
	}
}

// EncodePCMU converts a linear PCM sample to the nearest representable
// G.711 µ-law byte.
func EncodePCMU(sample int16) byte {
	i := sort.Search(256, func(i int) bool { return mulawLevels[i] >= sample })
	if i == 0 {
		return mulawLevelBytes[0]
	}
	if i == 256 {
		return mulawLevelBytes[255]
	}
	if int(mulawLevels[i])-int(sample) < int(sample)-int(mulawLevels[i-1]) {
		return mulawLevelBytes[i]
	}
	return mulawLevelBytes[i-1]
}

// recorderState is a presence-based audio recorder for a single station: it
// makes no attempt to detect speech vs. silence (VAD/energy thresholding was
// removed — the upstream devices already gate transmission with their own
// VOX/squelch, so every packet g711-radio receives is assumed to already be
// a real transmission). A WAV recording starts the moment any packet
// arrives, and every subsequent packet's samples are appended to the same
// in-progress recording back-to-back, in the order received, with no
// silence or other padding inserted between them regardless of real-world
// arrival timing — this is a UDP stream with no delivery-time guarantees,
// so the recording is simply the concatenation of payload audio, not an
// attempt to reconstruct a real-time timeline. A gap of up to gapLimit
// between packets is bridged into the same file (i.e. still just appended,
// with nothing inserted for the elapsed time) rather than starting a new
// recording; only a gap longer than gapLimit — or hitting maxClip —
// finishes the current recording, and the next packet starts a new one.
type recorderState struct {
	mu       sync.Mutex
	gapLimit time.Duration // bridge gaps up to this long into the same recording
	maxClip  time.Duration // hard cap on a single recording's length

	recording    bool
	lastPacketAt time.Time
	clipBuf      []int16
	clipStart    time.Time
	timer        *time.Timer
	onClip       func(samples []int16, start time.Time)
}

func newRecorderState(gapLimit, maxClip time.Duration, onClip func([]int16, time.Time)) *recorderState {
	return &recorderState{
		gapLimit: gapLimit,
		maxClip:  maxClip,
		onClip:   onClip,
	}
}

// Push appends one decoded PCM frame to the current recording, starting a
// new one if none is in progress, and arming a timer so the recording is
// finalized on its own if no further packets arrive within gapLimit. There's
// no other event to drive this: unlike the old VAD (which was fed continuous
// frames and could rely on always being Push'd, even during silence),
// presence-based recording only gets called when a real packet arrives, so
// the *absence* of a call has to be detected with a timer instead. Frames
// are appended exactly as received, back-to-back, with no gap-filling —
// see the recorderState doc comment for why.
func (r *recorderState) Push(samples []int16, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.timer != nil {
		r.timer.Stop()
	}

	if !r.recording {
		r.recording = true
		r.clipStart = now
		r.clipBuf = r.clipBuf[:0]
	}
	r.lastPacketAt = now
	r.clipBuf = append(r.clipBuf, samples...)

	clipDur := time.Duration(len(r.clipBuf)) * time.Second / recSampleRate
	if clipDur >= r.maxClip {
		r.finaliseLocked()
		return
	}

	r.timer = time.AfterFunc(r.gapLimit, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.finaliseLocked()
	})
}

// finaliseLocked must be called with mu held.
func (r *recorderState) finaliseLocked() {
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	if !r.recording || len(r.clipBuf) == 0 {
		r.recording = false
		r.clipBuf = r.clipBuf[:0]
		return
	}

	samples := make([]int16, len(r.clipBuf))
	copy(samples, r.clipBuf)
	start := r.clipStart

	r.recording = false
	r.clipBuf = r.clipBuf[:0]

	r.onClip(samples, start)
}
