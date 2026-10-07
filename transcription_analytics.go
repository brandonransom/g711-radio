package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

type whisperResult struct {
	Text             string   `json:"text"`
	Error            string   `json:"error"`
	Model            string   `json:"model"`
	Confidence       *float64 `json:"confidence"`
	AvgLogprob       *float64 `json:"avg_logprob"`
	TokenProbability *float64 `json:"-"`
	Segments         []struct {
		Confidence *float64 `json:"confidence"`
		AvgLogprob *float64 `json:"avg_logprob"`
		Words      []struct {
			Probability *float64 `json:"probability"`
		} `json:"words"`
	} `json:"segments"`
	AudioMs int64 `json:"-"`
}

// Confidence is used only when explicitly reported on a 0..1 scale.
// Average log probability is a separate diagnostic, not an accuracy score.
func (r *whisperResult) summarizeMetadata() {
	mean := func(top *float64, values []*float64, valid func(float64) bool) *float64 {
		if top != nil && valid(*top) {
			return top
		}
		var sum float64
		var count int
		for _, v := range values {
			if v != nil && valid(*v) {
				sum += *v
				count++
			}
		}
		if count == 0 {
			return nil
		}
		value := sum / float64(count)
		return &value
	}
	var confidence, logprob, probabilities []*float64
	for _, s := range r.Segments {
		confidence = append(confidence, s.Confidence)
		logprob = append(logprob, s.AvgLogprob)
		for _, word := range s.Words {
			probabilities = append(probabilities, word.Probability)
		}
	}
	r.Confidence = mean(r.Confidence, confidence, func(v float64) bool {
		return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 1
	})
	r.AvgLogprob = mean(r.AvgLogprob, logprob, func(v float64) bool {
		return !math.IsNaN(v) && !math.IsInf(v, 0) && v <= 0
	})
	r.TokenProbability = mean(nil, probabilities, func(v float64) bool {
		return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 1
	})
}

type transcriptionAttempt struct {
	Host             string   `json:"host"`
	Model            string   `json:"model,omitempty"`
	Outcome          string   `json:"outcome"`
	Ms               int64    `json:"ms"`
	QueueMs          int64    `json:"queueMs"`
	AudioMs          int64    `json:"audioMs,omitempty"`
	NoSpeech         bool     `json:"noSpeech,omitempty"`
	Filtered         bool     `json:"filtered,omitempty"`
	Confidence       *float64 `json:"confidence,omitempty"`
	AvgLogprob       *float64 `json:"avgLogprob,omitempty"`
	TokenProbability *float64 `json:"tokenProbability,omitempty"`
}

type transcriptionAgg struct {
	Attempts            int            `json:"attempts"`
	Succeeded           int            `json:"succeeded"`
	Failed              int            `json:"failed"`
	Retries             int            `json:"retries"`
	NoSpeech            int            `json:"noSpeech"`
	Filtered            int            `json:"filtered"`
	Ms                  int64          `json:"ms"`
	QueueMs             int64          `json:"queueMs"`
	AudioMs             int64          `json:"audioMs"`
	AudioWorkMs         int64          `json:"audioWorkMs"`
	ConfidenceSum       float64        `json:"confidenceSum"`
	ConfidenceN         int            `json:"confidenceN"`
	LogprobSum          float64        `json:"logprobSum"`
	LogprobN            int            `json:"logprobN"`
	TokenProbabilitySum float64        `json:"tokenProbabilitySum"`
	TokenProbabilityN   int            `json:"tokenProbabilityN"`
	Models              map[string]int `json:"models,omitempty"`
}

func (a *transcriptionAgg) add(t transcriptionAttempt) {
	a.Attempts++
	a.QueueMs += t.QueueMs
	if t.Model != "" {
		if a.Models == nil {
			a.Models = map[string]int{}
		}
		a.Models[t.Model]++
	}
	switch t.Outcome {
	case "retry":
		a.Retries++
	case "failed":
		a.Failed++
	case "success":
		a.Succeeded++
		a.Ms += t.Ms
		if t.AudioMs > 0 {
			a.AudioMs += t.AudioMs
			a.AudioWorkMs += t.Ms
		}
		if t.NoSpeech {
			a.NoSpeech++
		}
		if t.Filtered {
			a.Filtered++
		}
		if t.Confidence != nil {
			a.ConfidenceSum += *t.Confidence
			a.ConfidenceN++
		}
		if t.AvgLogprob != nil {
			a.LogprobSum += *t.AvgLogprob
			a.LogprobN++
		}
		if t.TokenProbability != nil {
			a.TokenProbabilitySum += *t.TokenProbability
			a.TokenProbabilityN++
		}
	}
}

func (a *transcriptionAgg) merge(b *transcriptionAgg) {
	a.Attempts += b.Attempts
	a.Succeeded += b.Succeeded
	a.Failed += b.Failed
	a.Retries += b.Retries
	a.NoSpeech += b.NoSpeech
	a.Filtered += b.Filtered
	a.Ms += b.Ms
	a.QueueMs += b.QueueMs
	a.AudioMs += b.AudioMs
	a.AudioWorkMs += b.AudioWorkMs
	a.ConfidenceSum += b.ConfidenceSum
	a.ConfidenceN += b.ConfidenceN
	a.LogprobSum += b.LogprobSum
	a.LogprobN += b.LogprobN
	a.TokenProbabilitySum += b.TokenProbabilitySum
	a.TokenProbabilityN += b.TokenProbabilityN
	if a.Models == nil {
		a.Models = map[string]int{}
	}
	addMap(a.Models, b.Models)
}

func (a *analyticsStore) TranscriptionAttempt(t transcriptionAttempt) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.appendLocked(analyticsEvent{Time: a.now(), Type: evTranscription, Transcription: &t})
}

type transcriptionHost struct {
	Host, Model string
	Ready, Busy bool
	Checked     time.Time
}

type transcriptionSnapshot struct {
	Enabled bool
	Queued  int
	Hosts   []transcriptionHost
}

func (e *whisperEndpoint) markChecked() {
	e.mu.Lock()
	e.checked = time.Now()
	e.mu.Unlock()
}

func (p *whisperPool) probeRemote(ep *whisperEndpoint) error {
	err := checkWhisperHealth(p.ctx, ep.baseURL, 5*time.Second)
	if p.ctx.Err() == nil {
		ep.markChecked()
		ep.setReady(err == nil)
	}
	return err
}

// Refresh idle remote hosts as well as active ones; inference alone cannot
// detect a host that went offline while the queue was empty.
func (p *whisperPool) monitorRemote(ep *whisperEndpoint) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			err := p.probeRemote(ep)
			if p.ctx.Err() != nil {
				return
			}
			if err != nil {
				p.logger.Printf("whisper %s: health check failed: %v", ep.label, err)
			}
		}
	}
}

func (p *whisperPool) transcriptionSnapshot() transcriptionSnapshot {
	if p == nil {
		return transcriptionSnapshot{}
	}
	s := transcriptionSnapshot{Enabled: true, Queued: p.queueDepth()}
	for _, ep := range p.endpoints {
		ep.mu.Lock()
		s.Hosts = append(s.Hosts, transcriptionHost{
			Host: ep.baseURL, Model: ep.model, Ready: ep.ready, Busy: ep.busy, Checked: ep.checked,
		})
		ep.mu.Unlock()
	}
	return s
}

type transcriptionRow struct {
	Host, Status, Model, RangeModels, Checked                                          string
	Attempts, Succeeded, Failed, Retries, NoSpeech, Filtered                           int
	Average, QueueAverage, RTF, Confidence, TokenProbability, Logprob, Share, Relative string
}

type transcriptionData struct {
	Enabled                             bool
	Configured, Available, Busy, Queued int
	Total                               transcriptionRow
	Hosts                               []transcriptionRow
}

func transcriptionMetrics(host string, a *transcriptionAgg) transcriptionRow {
	r := transcriptionRow{
		Host: host, Attempts: a.Attempts, Succeeded: a.Succeeded, Failed: a.Failed,
		Retries: a.Retries, NoSpeech: a.NoSpeech, Filtered: a.Filtered,
		Average: "-", QueueAverage: "-", RTF: "-", Relative: "-",
		Confidence: "not reported", Logprob: "not reported", Share: "-",
		TokenProbability: "not reported",
		Model:            "not reported", Status: "not configured", Checked: "-",
	}
	if a.Succeeded > 0 {
		r.Average = fmt.Sprintf("%.2fs", float64(a.Ms)/float64(a.Succeeded)/1000)
	}
	if a.Attempts > 0 {
		r.QueueAverage = fmt.Sprintf("%.2fs", float64(a.QueueMs)/float64(a.Attempts)/1000)
	}
	if a.AudioMs > 0 {
		r.RTF = fmt.Sprintf("%.3f", float64(a.AudioWorkMs)/float64(a.AudioMs))
	}
	if a.ConfidenceN > 0 {
		r.Confidence = fmt.Sprintf("%.1f%% (%d clips)", 100*a.ConfidenceSum/float64(a.ConfidenceN), a.ConfidenceN)
	}
	if a.LogprobN > 0 {
		r.Logprob = fmt.Sprintf("%.3f (%d clips)", a.LogprobSum/float64(a.LogprobN), a.LogprobN)
	}
	if a.TokenProbabilityN > 0 {
		r.TokenProbability = fmt.Sprintf("%.1f%% (%d clips)", 100*a.TokenProbabilitySum/float64(a.TokenProbabilityN), a.TokenProbabilityN)
	}
	var models []string
	for model := range a.Models {
		models = append(models, model)
	}
	sort.Strings(models)
	r.RangeModels = strings.Join(models, ", ")
	if r.RangeModels == "" {
		r.RangeModels = "not reported"
	}
	return r
}

func makeTranscriptionData(days []dayAgg, live transcriptionSnapshot) transcriptionData {
	d := transcriptionData{Enabled: live.Enabled, Configured: len(live.Hosts), Queued: live.Queued}
	hosts := map[string]*transcriptionAgg{}
	for _, day := range days {
		for host, a := range day.Transcription {
			if hosts[host] == nil {
				hosts[host] = &transcriptionAgg{}
			}
			hosts[host].merge(a)
		}
	}
	current := map[string]transcriptionHost{}
	for _, h := range live.Hosts {
		current[h.Host] = h
		if hosts[h.Host] == nil {
			hosts[h.Host] = &transcriptionAgg{}
		}
		if h.Ready {
			d.Available++
		}
		if h.Busy {
			d.Busy++
		}
	}
	total := &transcriptionAgg{}
	for _, a := range hosts {
		total.merge(a)
	}
	d.Total = transcriptionMetrics("", total)
	for host, a := range hosts {
		r := transcriptionMetrics(host, a)
		if h, ok := current[host]; ok {
			r.Status = "unavailable / starting"
			if h.Ready {
				r.Status = "ready"
			}
			if h.Busy {
				r.Status = "busy"
			}
			if h.Model != "" {
				r.Model = h.Model
			}
			if !h.Checked.IsZero() {
				r.Checked = h.Checked.Format("2006-01-02 15:04:05 MST")
			}
		}
		if total.Succeeded > 0 {
			r.Share = fmt.Sprintf("%.1f%%", 100*float64(a.Succeeded)/float64(total.Succeeded))
		}
		// Compare duration-normalized processing time, not raw latency:
		// hosts may receive very different clip lengths.
		othersAudio := total.AudioMs - a.AudioMs
		othersWork := total.AudioWorkMs - a.AudioWorkMs
		if a.AudioMs > 0 && a.AudioWorkMs > 0 && othersAudio > 0 && othersWork > 0 {
			r.Relative = fmt.Sprintf("%.2fx", (float64(othersWork)/float64(othersAudio))/(float64(a.AudioWorkMs)/float64(a.AudioMs)))
		}
		d.Hosts = append(d.Hosts, r)
	}
	sort.Slice(d.Hosts, func(i, j int) bool { return d.Hosts[i].Host < d.Hosts[j].Host })
	return d
}
