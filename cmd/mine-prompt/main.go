// mine-prompt turns the listener corrections collected in
// transcript-feedback.csv into something actionable: a measured error rate, a
// ranked list of the words whisper consistently gets wrong, and a drafted
// replacement for the "prompt" entry under whisper.inferenceParams in
// config.json.
//
// Whisper has no online learning, so corrections never feed back into the
// model on their own. The three things they *can* do are measure, diagnose,
// and prime:
//
//   - Measure. Word error rate against human-corrected text is the only way
//     to tell whether a model, beam-size, or prompt change actually helped
//     rather than just felt better.
//   - Diagnose. The substitution list ("heard X, was actually Y") shows
//     exactly which call signs, road numbers, and place names are being lost.
//   - Prime. Whisper's initial prompt is treated as fake preceding context,
//     so it biases decoding toward the vocabulary it contains. Feeding it
//     real corrected traffic is the highest-value knob available short of a
//     fine-tune.
//
// The drafted prompt is deliberately built from whole corrected transcripts
// rather than a bare word list. The prompt is conditioning context, not an
// instruction or a dictionary: whisper continues from it, so text that reads
// like real radio traffic primes both the vocabulary and the phrasing, while
// a comma-separated word list primes the model to emit comma-separated word
// lists.
//
// Usage:
//
//	go run ./cmd/mine-prompt -in D:\audio\transcript-feedback.csv
//	go run ./cmd/mine-prompt -in feedback.csv -stream "Pomeroy Net" -json
package main

import (
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strings"
	"unicode"
)

func main() {
	inPath := flag.String("in", "", "path to transcript-feedback.csv (required)")
	stream := flag.String("stream", "", "only consider feedback for this stream name (default: all streams)")
	top := flag.Int("top", 25, "how many missed words and confusions to list")
	minCount := flag.Int("min-count", 2, "ignore words missed fewer than this many times; one-offs are noise, not vocabulary")
	maxTokens := flag.Int("max-tokens", 224, "prompt budget in whisper tokens; whisper truncates the initial prompt at n_text_ctx/2, which is 224 for every current model")
	asJSON := flag.Bool("json", false, "print the drafted prompt as a config.json snippet ready to paste")
	flag.Parse()

	if *inPath == "" {
		flag.Usage()
		log.Fatal("-in is required")
	}

	rows, err := readFeedback(*inPath)
	if err != nil {
		log.Fatalf("read feedback: %v", err)
	}
	if *stream != "" {
		rows = filterStream(rows, *stream)
	}
	if len(rows) == 0 {
		log.Fatalf("no usable feedback rows in %s%s", *inPath, streamSuffix(*stream))
	}

	rep := analyze(rows, *minCount)
	if rep.refWords == 0 {
		log.Fatalf("no rated or corrected transcripts in %s%s; nothing to measure", *inPath, streamSuffix(*stream))
	}

	prompt := draftPrompt(rep, *maxTokens)

	if *asJSON {
		printJSON(prompt)
		return
	}
	printReport(os.Stdout, rep, prompt, *top, *maxTokens, *stream)
}

func streamSuffix(stream string) string {
	if stream == "" {
		return ""
	}
	return fmt.Sprintf(" for stream %q", stream)
}

// feedbackRow is one listener verdict, reduced to the fields this tool uses.
type feedbackRow struct {
	stream    string
	rating    string
	original  string
	corrected string
}

// readFeedback parses transcript-feedback.csv by header name rather than
// column position. The writing side documents its header as append-only, so
// name lookup keeps this tool working against both older and newer files.
func readFeedback(path string) ([]feedbackRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	// Rows are written with a fixed header but tolerate growth over time.
	r.FieldsPerRecord = -1

	header, err := r.Read()
	if err == io.EOF {
		return nil, fmt.Errorf("%s is empty", path)
	}
	if err != nil {
		return nil, err
	}
	index := make(map[string]int, len(header))
	for i, name := range header {
		index[strings.TrimSpace(name)] = i
	}
	for _, required := range []string{"streamName", "rating", "original", "corrected"} {
		if _, ok := index[required]; !ok {
			return nil, fmt.Errorf("%s has no %q column; is this a transcript-feedback.csv?", path, required)
		}
	}
	at := func(rec []string, name string) string {
		i, ok := index[name]
		if !ok || i >= len(rec) {
			return ""
		}
		return strings.TrimSpace(rec[i])
	}

	var rows []feedbackRow
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		rows = append(rows, feedbackRow{
			stream:    at(rec, "streamName"),
			rating:    at(rec, "rating"),
			original:  at(rec, "original"),
			corrected: at(rec, "corrected"),
		})
	}
	return rows, nil
}

func filterStream(rows []feedbackRow, stream string) []feedbackRow {
	out := rows[:0:0]
	for _, row := range rows {
		if strings.EqualFold(row.stream, stream) {
			out = append(out, row)
		}
	}
	return out
}

// reference returns the agreed-correct text for a row and whether the row is
// usable at all.
//
// A correction is ground truth. A row rated "good" with no correction is also
// ground truth — it asserts whisper's output was already right — and counting
// those is what keeps the error rate honest; dropping them would measure only
// the clips someone bothered to fix and overstate the error rate badly.
func (r feedbackRow) reference() (string, bool) {
	if r.corrected != "" {
		return r.corrected, true
	}
	if r.rating == "good" && r.original != "" {
		return r.original, true
	}
	return "", false
}

// confusion is one substitution whisper made: it heard `heard` where the
// listener says `actual` belonged.
type confusion struct {
	heard  string
	actual string
	count  int
}

type wordCount struct {
	word  string
	count int
}

type report struct {
	rows       int // rows in the file (after stream filtering)
	usable     int // rows carrying ground truth
	corrected  int // rows carrying an actual correction
	refWords   int // total words of ground truth
	errors     int // substitutions + deletions + insertions
	subs       int
	dels       int
	ins        int
	missed     []wordCount // words whisper failed to produce, most frequent first
	confusions []confusion // substitutions, most frequent first
	weight     map[string]int
	candidates []string // corrected transcripts, deduped, for prompt drafting
}

func (r report) wer() float64 {
	if r.refWords == 0 {
		return 0
	}
	return float64(r.errors) / float64(r.refWords)
}

// analyze aligns each transcript against its ground truth and accumulates
// error counts, missed vocabulary, and the confusion list.
func analyze(rows []feedbackRow, minCount int) report {
	rep := report{rows: len(rows), weight: map[string]int{}}
	missed := map[string]int{}
	confusions := map[confusion]int{}
	// Preserve the surface form (casing, digits) the listener actually typed
	// rather than the lowercased key used for matching, so the drafted
	// prompt reads like real traffic.
	surface := map[string]map[string]int{}
	seenCandidate := map[string]bool{}

	noteSurface := func(key, form string) {
		if surface[key] == nil {
			surface[key] = map[string]int{}
		}
		surface[key][form]++
	}

	for _, row := range rows {
		ref, ok := row.reference()
		if !ok {
			continue
		}
		rep.usable++
		if row.corrected != "" {
			rep.corrected++
			if !seenCandidate[row.corrected] {
				seenCandidate[row.corrected] = true
				rep.candidates = append(rep.candidates, row.corrected)
			}
		}

		refKeys, refForms := tokenize(ref)
		hypKeys, _ := tokenize(row.original)
		rep.refWords += len(refKeys)
		for i, key := range refKeys {
			noteSurface(key, refForms[i])
		}

		// A corrected row whose original was never recorded tells us the
		// right answer but not what whisper got wrong, so it can seed the
		// prompt but cannot contribute to the error rate.
		if row.original == "" {
			continue
		}

		for _, op := range align(hypKeys, refKeys) {
			switch op.kind {
			case opMatch:
				// nothing to learn
			case opSub:
				rep.subs++
				rep.errors++
				missed[refKeys[op.refIndex]]++
				confusions[confusion{heard: hypKeys[op.hypIndex], actual: refKeys[op.refIndex]}]++
			case opDel:
				rep.dels++
				rep.errors++
				missed[refKeys[op.refIndex]]++
			case opIns:
				rep.ins++
				rep.errors++
			}
		}
	}

	for word, count := range missed {
		if count < minCount {
			continue
		}
		rep.weight[word] = count
		rep.missed = append(rep.missed, wordCount{word: bestForm(surface, word), count: count})
	}
	sort.Slice(rep.missed, func(i, j int) bool {
		if rep.missed[i].count != rep.missed[j].count {
			return rep.missed[i].count > rep.missed[j].count
		}
		return rep.missed[i].word < rep.missed[j].word
	})

	for c, count := range confusions {
		if count < minCount {
			continue
		}
		rep.confusions = append(rep.confusions, confusion{
			heard:  c.heard,
			actual: bestForm(surface, c.actual),
			count:  count,
		})
	}
	sort.Slice(rep.confusions, func(i, j int) bool {
		if rep.confusions[i].count != rep.confusions[j].count {
			return rep.confusions[i].count > rep.confusions[j].count
		}
		if rep.confusions[i].actual != rep.confusions[j].actual {
			return rep.confusions[i].actual < rep.confusions[j].actual
		}
		return rep.confusions[i].heard < rep.confusions[j].heard
	})

	return rep
}

// bestForm returns the most common surface spelling recorded for a
// normalized word key, falling back to the key itself.
func bestForm(surface map[string]map[string]int, key string) string {
	forms := surface[key]
	if len(forms) == 0 {
		return key
	}
	best, bestN := key, -1
	for form, n := range forms {
		if n > bestN || (n == bestN && form < best) {
			best, bestN = form, n
		}
	}
	return best
}

// tokenize splits text into words, returning both a normalized matching key
// and the original surface form for each. Surrounding punctuation is stripped
// so "632." and "632" compare equal, but internal characters are kept so
// "mile-marker" and "I-90" survive intact.
func tokenize(s string) (keys, forms []string) {
	for _, field := range strings.Fields(s) {
		form := strings.TrimFunc(field, func(r rune) bool {
			return unicode.IsPunct(r) || unicode.IsSymbol(r)
		})
		if form == "" {
			continue
		}
		keys = append(keys, strings.ToLower(form))
		forms = append(forms, form)
	}
	return keys, forms
}

type opKind int

const (
	opMatch opKind = iota
	opSub
	opDel // in the reference, missing from the transcript
	opIns // in the transcript, not in the reference
)

type alignOp struct {
	kind     opKind
	hypIndex int
	refIndex int
}

// align computes the standard word-level edit alignment between whisper's
// output and the reference, which is what word error rate is defined over and
// what makes substitutions distinguishable from deletions. Transcripts here
// are a sentence or two — the writing side clamps each field to a few
// kilobytes — so the quadratic table is comfortably small.
func align(hyp, ref []string) []alignOp {
	n, m := len(hyp), len(ref)
	cost := make([][]int, n+1)
	for i := range cost {
		cost[i] = make([]int, m+1)
		cost[i][0] = i
	}
	for j := 0; j <= m; j++ {
		cost[0][j] = j
	}
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			sub := cost[i-1][j-1]
			if hyp[i-1] != ref[j-1] {
				sub++
			}
			best := sub
			if d := cost[i-1][j] + 1; d < best {
				best = d
			}
			if d := cost[i][j-1] + 1; d < best {
				best = d
			}
			cost[i][j] = best
		}
	}

	var ops []alignOp
	i, j := n, m
	for i > 0 || j > 0 {
		switch {
		case i > 0 && j > 0 && hyp[i-1] == ref[j-1] && cost[i][j] == cost[i-1][j-1]:
			ops = append(ops, alignOp{kind: opMatch, hypIndex: i - 1, refIndex: j - 1})
			i, j = i-1, j-1
		case i > 0 && j > 0 && cost[i][j] == cost[i-1][j-1]+1:
			ops = append(ops, alignOp{kind: opSub, hypIndex: i - 1, refIndex: j - 1})
			i, j = i-1, j-1
		case j > 0 && cost[i][j] == cost[i][j-1]+1:
			ops = append(ops, alignOp{kind: opDel, hypIndex: i - 1, refIndex: j - 1})
			j--
		default:
			ops = append(ops, alignOp{kind: opIns, hypIndex: i - 1, refIndex: j - 1})
			i--
		}
	}
	// Backtracking produced the alignment in reverse.
	for l, r := 0, len(ops)-1; l < r; l, r = l+1, r-1 {
		ops[l], ops[r] = ops[r], ops[l]
	}
	return ops
}

// draftPrompt picks the set of corrected transcripts that covers as much
// missed vocabulary as possible within the token budget.
//
// This is greedy maximum coverage: repeatedly take the transcript carrying
// the most still-uncovered error weight. Choosing whole transcripts rather
// than assembling a word list matters, because whisper treats the prompt as
// preceding context and continues its style — real traffic primes real
// traffic. Once a word is covered its weight drops to zero, so the selection
// spreads across distinct vocabulary instead of stacking near-duplicates.
func draftPrompt(rep report, maxTokens int) string {
	remaining := make(map[string]int, len(rep.weight))
	for word, weight := range rep.weight {
		remaining[word] = weight
	}

	var chosen []string
	used := 0
	picked := make([]bool, len(rep.candidates))

	for {
		bestIdx, bestScore := -1, 0
		for i, cand := range rep.candidates {
			if picked[i] {
				continue
			}
			if used+estimateTokens(cand) > maxTokens {
				continue
			}
			keys, _ := tokenize(cand)
			seen := map[string]bool{}
			score := 0
			for _, key := range keys {
				if seen[key] {
					continue
				}
				seen[key] = true
				score += remaining[key]
			}
			if score > bestScore {
				bestIdx, bestScore = i, score
			}
		}
		if bestIdx < 0 {
			break
		}
		cand := rep.candidates[bestIdx]
		picked[bestIdx] = true
		chosen = append(chosen, cand)
		used += estimateTokens(cand)
		keys, _ := tokenize(cand)
		for _, key := range keys {
			delete(remaining, key)
		}
	}

	// Nothing scored: either there is no measured error weight yet, or every
	// correction is longer than the budget. Fall back to the shortest
	// corrections, which still prime domain vocabulary and phrasing.
	if len(chosen) == 0 {
		order := make([]string, len(rep.candidates))
		copy(order, rep.candidates)
		sort.Slice(order, func(i, j int) bool { return len(order[i]) < len(order[j]) })
		for _, cand := range order {
			if used+estimateTokens(cand) > maxTokens {
				continue
			}
			chosen = append(chosen, cand)
			used += estimateTokens(cand)
		}
	}

	return strings.Join(chosen, " ")
}

// estimateTokens approximates whisper's BPE token count.
//
// The real count needs whisper's tokenizer, which this tool deliberately does
// not depend on. English prose runs around four characters per token, but
// radio traffic is dense with digits and call signs that tokenize far worse,
// so this uses a deliberately pessimistic ratio: overshooting the 224-token
// limit means whisper silently truncates the prompt, while undershooting only
// wastes a little context.
func estimateTokens(s string) int {
	const charsPerToken = 3.0
	byChars := int(float64(len(s))/charsPerToken) + 1
	byWords := len(strings.Fields(s)) * 2
	if byWords > byChars {
		return byWords
	}
	return byChars
}

func printJSON(prompt string) {
	snippet := map[string]any{
		"whisper": map[string]any{
			"inferenceParams": map[string]any{
				"prompt": prompt,
			},
		},
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(snippet); err != nil {
		log.Fatalf("encode: %v", err)
	}
}

func printReport(w io.Writer, rep report, prompt string, top, maxTokens int, stream string) {
	fmt.Fprintf(w, "Transcript feedback analysis%s\n", streamSuffix(stream))
	fmt.Fprintf(w, "%s\n\n", strings.Repeat("=", 60))

	fmt.Fprintf(w, "Rows read:             %d\n", rep.rows)
	fmt.Fprintf(w, "Usable (ground truth): %d\n", rep.usable)
	fmt.Fprintf(w, "  with corrections:    %d\n", rep.corrected)
	fmt.Fprintf(w, "Reference words:       %d\n\n", rep.refWords)

	fmt.Fprintf(w, "Word error rate:       %.1f%%  (%d errors / %d words)\n", rep.wer()*100, rep.errors, rep.refWords)
	fmt.Fprintf(w, "  substitutions:       %d\n", rep.subs)
	fmt.Fprintf(w, "  deletions:           %d\n", rep.dels)
	fmt.Fprintf(w, "  insertions:          %d\n\n", rep.ins)

	if rep.usable < 50 {
		fmt.Fprintf(w, "NOTE: %d rated clips is a thin sample. Treat the rate as a rough\n", rep.usable)
		fmt.Fprintf(w, "      signal until there are a few hundred, and remember it is biased\n")
		fmt.Fprintf(w, "      toward clips someone felt moved to rate.\n\n")
	}

	fmt.Fprintf(w, "Most frequently missed words\n%s\n", strings.Repeat("-", 60))
	if len(rep.missed) == 0 {
		fmt.Fprintln(w, "  (none above the -min-count threshold)")
	}
	for i, m := range rep.missed {
		if i >= top {
			break
		}
		fmt.Fprintf(w, "  %4d  %s\n", m.count, m.word)
	}

	fmt.Fprintf(w, "\nMost frequent confusions (heard -> actual)\n%s\n", strings.Repeat("-", 60))
	if len(rep.confusions) == 0 {
		fmt.Fprintln(w, "  (none above the -min-count threshold)")
	}
	for i, c := range rep.confusions {
		if i >= top {
			break
		}
		fmt.Fprintf(w, "  %4d  %-24s -> %s\n", c.count, c.heard, c.actual)
	}

	fmt.Fprintf(w, "\nDrafted prompt (~%d of %d tokens, estimated)\n%s\n", estimateTokens(prompt), maxTokens, strings.Repeat("-", 60))
	if prompt == "" {
		fmt.Fprintln(w, "  (no corrections available to build one from)")
	} else {
		fmt.Fprintf(w, "%s\n", prompt)
	}

	fmt.Fprintf(w, "\n%s\n", strings.Repeat("=", 60))
	fmt.Fprintln(w, "Re-run with -json to emit this as a config.json snippet.")
	fmt.Fprintln(w, "Read the draft before using it. The prompt is preceding context, not")
	fmt.Fprintln(w, "an instruction: whisper will happily hallucinate phrases from it into")
	fmt.Fprintln(w, "silent or noisy clips, so keep it representative rather than long.")
	fmt.Fprintln(w, "Then re-measure with this same tool to confirm the rate actually fell.")
}
