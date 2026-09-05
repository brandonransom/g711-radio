// mulaw-to-wav converts a raw G.711 µ-law byte stream (as produced by the
// station.audioDumpDir diagnostic feature, or any other raw µ-law capture)
// into a playable 16-bit PCM WAV file, for offline listening/comparison.
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"log"
	"os"
)

func main() {
	inPath := flag.String("in", "", "input file containing raw G.711 µ-law bytes (required)")
	outPath := flag.String("out", "", "output WAV path (required)")
	sampleRate := flag.Int("rate", 8000, "sample rate of the µ-law audio")
	flag.Parse()

	if *inPath == "" || *outPath == "" {
		log.Fatal("both -in and -out are required")
	}

	mulaw, err := os.ReadFile(*inPath)
	if err != nil {
		log.Fatalf("read %s: %v", *inPath, err)
	}

	wav, err := encodeMulawAsWAV(mulaw, *sampleRate)
	if err != nil {
		log.Fatalf("encode WAV: %v", err)
	}

	if err := os.WriteFile(*outPath, wav, 0644); err != nil {
		log.Fatalf("write %s: %v", *outPath, err)
	}

	log.Printf("wrote %s: %d µ-law bytes -> %.2fs of audio at %d Hz", *outPath, len(mulaw), float64(len(mulaw))/float64(*sampleRate), *sampleRate)
}

// pcmuTable is the standard G.711 µ-law -> 16-bit linear PCM expansion table.
var pcmuTable [256]int16

func init() {
	const bias = 33
	for i := 0; i < 256; i++ {
		b := ^byte(i)
		sign := b & 0x80
		exp := (b >> 4) & 0x07
		mantissa := b & 0x0F
		sample := int16((int(mantissa)<<1 | 1) << (int(exp) + 2))
		sample += bias
		if sign == 0 {
			sample = -sample
		}
		pcmuTable[i] = sample
	}
}

func encodeMulawAsWAV(mulaw []byte, sampleRate int) ([]byte, error) {
	dataSize := len(mulaw) * 2
	fileSize := 36 + dataSize

	var buf bytes.Buffer
	buf.Grow(fileSize + 8)
	write := func(v any) { _ = binary.Write(&buf, binary.LittleEndian, v) }

	buf.WriteString("RIFF")
	write(uint32(fileSize))
	buf.WriteString("WAVE")

	buf.WriteString("fmt ")
	write(uint32(16))
	write(uint16(1))
	write(uint16(1))
	write(uint32(sampleRate))
	write(uint32(sampleRate * 2))
	write(uint16(2))
	write(uint16(16))

	buf.WriteString("data")
	write(uint32(dataSize))
	for _, b := range mulaw {
		write(pcmuTable[b])
	}

	return buf.Bytes(), nil
}
