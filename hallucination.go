package main

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// noSpeechMarker is published when whisper returns nothing usable. Like
// every bracketed marker it is UI status, never written to the transcript
// archive (see transcriptHub.Publish).
const noSpeechMarker = "[no speech detected]"

// Whisper "hallucinates" text from its training data — mostly subtitle and
// video credits — when given silence, static or squelch. These are removed
// wherever they appear in a transcript. Keep them long and specific: a
// short phrase here could delete real radio traffic.
var defaultHallucinationPhrases = []string{
	"copyright australian broadcasting corporation",
	"subtitles by the amara org community",
	"subtitles by the amara org",
	"subtitled by the amara org community",
	"amara org",
	"transcription by castingwords",
	"transcription by esotranscription",
	"thank you for watching",
	"thanks for watching",
	"thank you so much for watching",
	"thank you very much for watching",
	"thanks for watching and see you next time",
	"please subscribe to my channel",
	"please like and subscribe",
	"like and subscribe",
	"don t forget to like and subscribe",
	"subscribe to my channel",
	"see you in the next video",
	"i ll see you in the next video",
	"www mooji org",
	"subs by www zeoranger co uk",
	"subtitles by steamteam",
	"subtitles made by the community of amara org",
	"ご視聴ありがとうございました",
}

// These are only treated as hallucinations when they are the entire
// transcript: "you" alone is a classic silence hallucination, but the word
// inside real traffic must survive.
var defaultHallucinationExactPhrases = []string{
	"you",
	"thank you",
	"thank you very much",
	"thank you so much",
	"thanks",
	"bye",
	"bye bye",
	"so",
	"the end",
	"music",
	"silence",
}

// hallucinationFilterConfig configures hallucinationFilter.
type hallucinationFilterConfig struct {
	// UseDefaults keeps the built-in phrase lists (default true).
	UseDefaults *bool `json:"useDefaults"`
	// Phrases are removed wherever they appear in a transcript.
	Phrases []string `json:"phrases"`
	// ExactPhrases are only removed when they are the whole transcript.
	ExactPhrases []string `json:"exactPhrases"`
	// Replacement is published when nothing is left after filtering.
	// Defaults to "[no speech detected]". Text starting with "[" is
	// treated as a status marker and kept out of transcripts.csv.
	Replacement string `json:"replacement"`
}

type hallucinationFilter struct {
	phrases     []*regexp.Regexp
	exact       map[string]bool
	replacement string
}

func newHallucinationFilter(cfg *hallucinationFilterConfig) (*hallucinationFilter, error) {
	if cfg == nil {
		cfg = &hallucinationFilterConfig{}
	}
	f := &hallucinationFilter{exact: make(map[string]bool), replacement: noSpeechMarker}
	if strings.TrimSpace(cfg.Replacement) != "" {
		f.replacement = strings.TrimSpace(cfg.Replacement)
	}
	phrases, exact := cfg.Phrases, cfg.ExactPhrases
	if cfg.UseDefaults == nil || *cfg.UseDefaults {
		phrases = append(append([]string(nil), defaultHallucinationPhrases...), phrases...)
		exact = append(append([]string(nil), defaultHallucinationExactPhrases...), exact...)
	}
	for _, p := range phrases {
		words := strings.Fields(normalizeTranscript(p))
		if len(words) == 0 {
			return nil, fmt.Errorf("phrases: %q has no letters or digits", p)
		}
		for i, w := range words {
			words[i] = regexp.QuoteMeta(w)
		}
		// Words may be separated by any punctuation/space, and trailing
		// punctuation goes with the phrase ("Thanks for watching!").
		pattern := `(?i)(^|[^\pL\pN])` + strings.Join(words, `[^\pL\pN]+`) + `([^\pL\pN\s]*)($|[^\pL\pN])`
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("phrases: %q: %w", p, err)
		}
		f.phrases = append(f.phrases, re)
	}
	for _, p := range exact {
		n := normalizeTranscript(p)
		if n == "" {
			return nil, fmt.Errorf("exactPhrases: %q has no letters or digits", p)
		}
		f.exact[n] = true
	}
	return f, nil
}

// normalizeTranscript lowercases text and reduces everything that isn't a
// letter or digit to single spaces, so matching ignores punctuation.
func normalizeTranscript(s string) string {
	var b strings.Builder
	space := true
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			space = false
		} else if !space {
			b.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(b.String())
}

// Apply returns text with hallucinated phrases removed, the replacement
// marker if nothing real is left, and whether anything was filtered.
func (f *hallucinationFilter) Apply(text string) (string, bool) {
	if f == nil || text == "" {
		return text, false
	}
	out := text
	for _, re := range f.phrases {
		// Loop because adjacent matches share the separator character.
		for {
			next := re.ReplaceAllString(out, "$1$3")
			if next == out {
				break
			}
			out = next
		}
	}
	norm := normalizeTranscript(out)
	if norm == "" || f.exact[norm] {
		return f.replacement, true
	}
	if out == text {
		return text, false
	}
	return strings.Join(strings.Fields(out), " "), true
}
