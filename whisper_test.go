package main

import "testing"

func TestShouldAutoTranscribeClipLengthWindow(t *testing.T) {
	tests := []struct {
		name       string
		cfg        *whisperConfig
		durationMs int
		want       bool
	}{
		{
			name:       "nil config never auto-transcribes",
			cfg:        nil,
			durationMs: 30000,
			want:       false,
		},
		{
			name:       "unset minimum disables automatic transcription",
			cfg:        &whisperConfig{},
			durationMs: 30000,
			want:       false,
		},
		{
			name:       "shorter than minimum is skipped",
			cfg:        &whisperConfig{AutoTranscribeMinClipMs: 20000},
			durationMs: 19999,
			want:       false,
		},
		{
			name:       "exactly the minimum qualifies",
			cfg:        &whisperConfig{AutoTranscribeMinClipMs: 20000},
			durationMs: 20000,
			want:       true,
		},
		{
			name:       "no maximum means no upper bound",
			cfg:        &whisperConfig{AutoTranscribeMinClipMs: 20000},
			durationMs: 60 * 60 * 1000,
			want:       true,
		},
		{
			name:       "exactly the maximum qualifies",
			cfg:        &whisperConfig{AutoTranscribeMinClipMs: 20000, AutoTranscribeMaxClipMs: 120000},
			durationMs: 120000,
			want:       true,
		},
		{
			name:       "longer than maximum is skipped",
			cfg:        &whisperConfig{AutoTranscribeMinClipMs: 20000, AutoTranscribeMaxClipMs: 120000},
			durationMs: 120001,
			want:       false,
		},
		{
			name:       "inside the window qualifies",
			cfg:        &whisperConfig{AutoTranscribeMinClipMs: 20000, AutoTranscribeMaxClipMs: 120000},
			durationMs: 45000,
			want:       true,
		},
		{
			// Contradictory bounds can't be satisfied; startup logs a warning.
			name:       "maximum below minimum excludes everything",
			cfg:        &whisperConfig{AutoTranscribeMinClipMs: 60000, AutoTranscribeMaxClipMs: 30000},
			durationMs: 45000,
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldAutoTranscribe(tt.cfg, tt.durationMs); got != tt.want {
				t.Fatalf("shouldAutoTranscribe(%+v, %d) = %v, want %v", tt.cfg, tt.durationMs, got, tt.want)
			}
		})
	}
}

// A negative maximum is normalized to zero ("no upper bound") rather than
// silently rejecting every clip.
func TestSetDefaultsNormalizesNegativeAutoTranscribeBounds(t *testing.T) {
	cfg := &whisperConfig{AutoTranscribeMinClipMs: -1, AutoTranscribeMaxClipMs: -1}
	cfg.setDefaults()

	if cfg.AutoTranscribeMinClipMs != 0 {
		t.Fatalf("AutoTranscribeMinClipMs = %d, want 0", cfg.AutoTranscribeMinClipMs)
	}
	if cfg.AutoTranscribeMaxClipMs != 0 {
		t.Fatalf("AutoTranscribeMaxClipMs = %d, want 0", cfg.AutoTranscribeMaxClipMs)
	}

	withMin := &whisperConfig{AutoTranscribeMinClipMs: 20000, AutoTranscribeMaxClipMs: -5}
	withMin.setDefaults()
	if !shouldAutoTranscribe(withMin, 10*60*1000) {
		t.Fatal("a negative maximum should behave as no upper bound")
	}
}
