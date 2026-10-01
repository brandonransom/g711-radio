package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCSV(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "transcript-feedback.csv")
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

const csvHeader = "timestamp,filename,clipId,streamName,stateName,groupName,audioUrl,rating,original,corrected,clientIp\n"

func TestReadFeedbackUsesHeaderNames(t *testing.T) {
	// Columns deliberately reordered and an unknown one appended: the
	// reader must locate fields by name, not position.
	path := writeCSV(t, "corrected,original,rating,streamName,newColumn\n"+
		"Engine 632,engine six thirty two,bad,Pomeroy Net,ignored\n")

	rows, err := readFeedback(path)
	if err != nil {
		t.Fatalf("readFeedback: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	want := feedbackRow{stream: "Pomeroy Net", rating: "bad", original: "engine six thirty two", corrected: "Engine 632"}
	if rows[0] != want {
		t.Errorf("row = %+v, want %+v", rows[0], want)
	}
}

func TestReadFeedbackRejectsForeignCSV(t *testing.T) {
	path := writeCSV(t, "filename,streamName,transcript\nclip.wav,Pomeroy Net,hello\n")
	if _, err := readFeedback(path); err == nil {
		t.Fatal("expected an error for a non-feedback CSV")
	}
}

func TestAlignClassifiesEditOperations(t *testing.T) {
	hyp, _ := tokenize("engine six thirty two to dispatch now")
	ref, _ := tokenize("Engine 632 to dispatch")

	var subs, dels, ins, matches int
	for _, op := range align(hyp, ref) {
		switch op.kind {
		case opMatch:
			matches++
		case opSub:
			subs++
		case opDel:
			dels++
		case opIns:
			ins++
		}
	}
	// "engine" (case-insensitively), "to" and "dispatch" line up.
	if matches != 3 {
		t.Errorf("matches = %d, want 3", matches)
	}
	// "six thirty two" must collapse into "632" — one substitution plus two
	// insertions — and the trailing "now" is a third insertion.
	if subs != 1 || ins != 3 || dels != 0 {
		t.Errorf("sub %d, ins %d, del %d; want 1/3/0", subs, ins, dels)
	}
}

// Casing is a style difference, not a transcription error, so normalizing it
// away keeps the rate focused on words actually misheard.
func TestAlignIgnoresCaseOnlyDifferences(t *testing.T) {
	hyp, _ := tokenize("engine 632 to dispatch")
	ref, _ := tokenize("Engine 632 to dispatch")
	for _, op := range align(hyp, ref) {
		if op.kind != opMatch {
			t.Fatalf("case-only difference produced a %v", op.kind)
		}
	}
}

func TestAlignIdenticalInputHasNoErrors(t *testing.T) {
	toks, _ := tokenize("Engine 632 to dispatch")
	for _, op := range align(toks, toks) {
		if op.kind != opMatch {
			t.Fatalf("unexpected op %v on identical input", op.kind)
		}
	}
}

func TestTokenizeStripsEdgePunctuationOnly(t *testing.T) {
	keys, forms := tokenize("Engine 632, en route to mile-marker 14.")
	wantKeys := []string{"engine", "632", "en", "route", "to", "mile-marker", "14"}
	if strings.Join(keys, "|") != strings.Join(wantKeys, "|") {
		t.Errorf("keys = %v, want %v", keys, wantKeys)
	}
	// Surface forms keep their original casing for prompt drafting.
	if forms[0] != "Engine" {
		t.Errorf("form[0] = %q, want Engine", forms[0])
	}
}

func TestGoodRatingsCountTowardErrorRate(t *testing.T) {
	// One clip wrong, nine clips confirmed correct. Counting only the
	// corrected row would report a wildly overstated error rate.
	body := csvHeader
	body += ",,,Pomeroy Net,,,,bad,engine six thirty two,Engine 632,\n"
	for i := 0; i < 9; i++ {
		body += ",,,Pomeroy Net,,,,good,Engine 632 to dispatch,,\n"
	}
	rows, err := readFeedback(writeCSV(t, body))
	if err != nil {
		t.Fatalf("readFeedback: %v", err)
	}

	rep := analyze(rows, 1)
	if rep.usable != 10 {
		t.Fatalf("usable = %d, want 10", rep.usable)
	}
	if rep.corrected != 1 {
		t.Fatalf("corrected = %d, want 1", rep.corrected)
	}
	// 9 good rows contribute 36 correct words, so the rate must be modest.
	if rep.wer() > 0.25 {
		t.Errorf("wer = %.2f, want <= 0.25 once confirmed-good clips are counted", rep.wer())
	}
}

func TestAnalyzeRanksMissedWordsAndConfusions(t *testing.T) {
	body := csvHeader
	for i := 0; i < 3; i++ {
		body += ",,,Pomeroy Net,,,,bad,engine 630 to dispatch,Engine 632 to dispatch,\n"
	}
	body += ",,,Pomeroy Net,,,,bad,seater creek fire,Cedar Creek fire,\n"
	rows, err := readFeedback(writeCSV(t, body))
	if err != nil {
		t.Fatalf("readFeedback: %v", err)
	}

	// min-count 2 must drop the single "Cedar" occurrence as noise while
	// keeping the thrice-repeated miss.
	rep := analyze(rows, 2)
	if len(rep.missed) != 1 || rep.missed[0].word != "632" {
		t.Fatalf("missed = %+v, want only 632", rep.missed)
	}
	if rep.missed[0].count != 3 {
		t.Errorf("count = %d, want 3", rep.missed[0].count)
	}
	if len(rep.confusions) != 1 {
		t.Fatalf("confusions = %+v, want 1", rep.confusions)
	}
	if rep.confusions[0].heard != "630" || rep.confusions[0].actual != "632" {
		t.Errorf("confusion = %+v", rep.confusions[0])
	}
}

func TestDraftPromptCoversDistinctVocabularyWithinBudget(t *testing.T) {
	body := csvHeader
	// Two different misses; the draft should reach for both rather than
	// taking two near-duplicates of the same one.
	for i := 0; i < 3; i++ {
		body += ",,,Net,,,,bad,engine 630 to dispatch,Engine 632 to dispatch,\n"
		body += ",,,Net,,,,bad,engine 630 responding,Engine 632 responding,\n"
		body += ",,,Net,,,,bad,seater creek fire road 25,Cedar Creek fire Road 25,\n"
	}
	rows, err := readFeedback(writeCSV(t, body))
	if err != nil {
		t.Fatalf("readFeedback: %v", err)
	}
	rep := analyze(rows, 2)

	prompt := draftPrompt(rep, 224)
	if prompt == "" {
		t.Fatal("prompt is empty")
	}
	if estimateTokens(prompt) > 224 {
		t.Errorf("prompt is over budget: %d tokens", estimateTokens(prompt))
	}
	for _, want := range []string{"Engine", "Cedar"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt %q is missing %q", prompt, want)
		}
	}
}

func TestDraftPromptRespectsTinyBudget(t *testing.T) {
	body := csvHeader +
		",,,Net,,,,bad,engine 632,Engine 632 to dispatch en route to the Cedar Creek fire,\n"
	rows, err := readFeedback(writeCSV(t, body))
	if err != nil {
		t.Fatalf("readFeedback: %v", err)
	}
	rep := analyze(rows, 1)

	// A budget nothing can fit must yield an empty prompt, never a
	// truncated fragment that would prime whisper with half a sentence.
	if got := draftPrompt(rep, 2); got != "" {
		t.Errorf("prompt = %q, want empty under a 2-token budget", got)
	}
}

func TestDraftPromptFallsBackWhenNoMeasuredErrors(t *testing.T) {
	// Corrections exist but no original was recorded, so nothing has error
	// weight. The draft should still use the corrections.
	body := csvHeader + ",,,Net,,,,bad,,Engine 632 to dispatch,\n"
	rows, err := readFeedback(writeCSV(t, body))
	if err != nil {
		t.Fatalf("readFeedback: %v", err)
	}
	rep := analyze(rows, 1)
	if rep.errors != 0 {
		t.Fatalf("errors = %d, want 0 when no original was recorded", rep.errors)
	}
	if got := draftPrompt(rep, 224); !strings.Contains(got, "Engine 632") {
		t.Errorf("prompt = %q, want the correction used as a fallback", got)
	}
}

func TestFilterStreamIsCaseInsensitive(t *testing.T) {
	rows := []feedbackRow{
		{stream: "Pomeroy Net", corrected: "a"},
		{stream: "Walla Walla Net", corrected: "b"},
	}
	got := filterStream(rows, "pomeroy net")
	if len(got) != 1 || got[0].corrected != "a" {
		t.Fatalf("filtered = %+v", got)
	}
}

func TestPrintReportRendersWithoutData(t *testing.T) {
	var sb strings.Builder
	printReport(&sb, report{}, "", 25, 224, "")
	out := sb.String()
	for _, want := range []string{"Word error rate", "min-count threshold", "no corrections available"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
}
