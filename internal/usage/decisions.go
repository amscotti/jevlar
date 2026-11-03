package usage

import (
	"encoding/json"
	"sort"
)

// Decision is one model answer reduced to a countable signal: which choice
// it picked, or how confident a yes/no was. Nothing else from the answer
// is retained.
type Decision struct {
	// Question is the question key from the response.
	Question string
	// Choice is the chosen criterion key (choice answers only).
	Choice string
	// Confidence is the confidence value in [0,1], bucketed when stored.
	Confidence float64
	// HasConfidence distinguishes an omitted confidence from a reported
	// zero, so "not reported" never lands in the lowest bucket.
	HasConfidence bool
	// Type is "choice" or "noul".
	Type string
}

// decisionsWire is the minimal response shape the decision parser reads.
type decisionsWire struct {
	Answers map[string]struct {
		Type       string   `json:"type"`
		Choice     string   `json:"choice"`
		Noul       float64  `json:"noul"`
		Confidence *float64 `json:"confidence"`
	} `json:"answers"`
}

// ParseDecisions extracts the answer distribution signals from a response
// body. It reads only `answers.<question>.type|choice|noul|confidence`;
// question text and state are never read.
func ParseDecisions(body []byte) []Decision {
	if len(body) == 0 {
		return nil
	}
	var w decisionsWire
	if err := json.Unmarshal(body, &w); err != nil || len(w.Answers) == 0 {
		return nil
	}
	out := make([]Decision, 0, len(w.Answers))
	for question, a := range w.Answers {
		switch a.Type {
		case "choice":
			d := Decision{Question: question, Type: "choice", Choice: a.Choice}
			if a.Confidence != nil {
				d.Confidence = *a.Confidence
				d.HasConfidence = true
			}
			out = append(out, d)
		case "noul":
			// Noul is the answer probability and is always present.
			out = append(out, Decision{Question: question, Type: "noul", Confidence: a.Noul, HasConfidence: true})
		default:
			// score and other shapes carry no discrete signal.
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Question < out[j].Question })
	return out
}

// ConfidenceBucket maps a probability to a fixed bucket label. Fixed
// buckets keep the rollup cardinality bounded no matter what the model
// reports.
func ConfidenceBucket(p float64) string {
	switch {
	case p < 0.2:
		return "0.0-0.2"
	case p < 0.4:
		return "0.2-0.4"
	case p < 0.6:
		return "0.4-0.6"
	case p < 0.8:
		return "0.6-0.8"
	default:
		return "0.8-1.0"
	}
}

// DecisionCounts is the per-model answer distribution. Both maps are
// keyed by question, then by choice or bucket label.
type DecisionCounts struct {
	// Choices counts discrete choice answers.
	Choices map[string]map[string]int64 `json:"choices,omitempty"`
	// Confidence counts bucketed confidence values.
	Confidence map[string]map[string]int64 `json:"confidence,omitempty"`
	// Total is the number of decisions observed.
	Total int64 `json:"total"`
}

// newDecisionCounts returns an empty distribution.
func newDecisionCounts() DecisionCounts {
	return DecisionCounts{Choices: map[string]map[string]int64{}, Confidence: map[string]map[string]int64{}}
}

// add folds one decision into the distribution.
func (d *DecisionCounts) add(dec Decision) {
	if d.Choices == nil {
		d.Choices = map[string]map[string]int64{}
	}
	if d.Confidence == nil {
		d.Confidence = map[string]map[string]int64{}
	}
	if dec.Type == "choice" && dec.Choice != "" {
		m := d.Choices[dec.Question]
		if m == nil {
			m = map[string]int64{}
			d.Choices[dec.Question] = m
		}
		m[dec.Choice]++
	}
	if dec.HasConfidence {
		m := d.Confidence[dec.Question]
		if m == nil {
			m = map[string]int64{}
			d.Confidence[dec.Question] = m
		}
		m[ConfidenceBucket(dec.Confidence)]++
	}
	d.Total++
}

// subtract returns a - b, used to derive one interval's delta.
func (d DecisionCounts) subtract(b DecisionCounts) DecisionCounts {
	out := newDecisionCounts()
	out.Total = d.Total - b.Total
	for q, m := range d.Choices {
		for k, v := range m {
			if delta := v - b.Choices[q][k]; delta != 0 {
				if out.Choices[q] == nil {
					out.Choices[q] = map[string]int64{}
				}
				out.Choices[q][k] = delta
			}
		}
	}
	for q, m := range d.Confidence {
		for k, v := range m {
			if delta := v - b.Confidence[q][k]; delta != 0 {
				if out.Confidence[q] == nil {
					out.Confidence[q] = map[string]int64{}
				}
				out.Confidence[q][k] = delta
			}
		}
	}
	return out
}

// RecordDecision folds one response's decisions into the per-model
// distribution.
func (t *Tracker) RecordDecision(model string, decisions []Decision) {
	if len(decisions) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	d := t.decisions[model]
	if d == nil {
		nd := newDecisionCounts()
		d = &nd
		t.decisions[model] = d
	}
	for _, dec := range decisions {
		d.add(dec)
	}
}

// DecisionsSnapshot returns a copy of the per-model distributions.
func (t *Tracker) DecisionsSnapshot() map[string]DecisionCounts {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]DecisionCounts, len(t.decisions))
	for model, d := range t.decisions {
		out[model] = cloneDecisionCounts(*d)
	}
	return out
}

func cloneDecisionCounts(d DecisionCounts) DecisionCounts {
	out := newDecisionCounts()
	out.Total = d.Total
	for q, m := range d.Choices {
		cp := make(map[string]int64, len(m))
		for k, v := range m {
			cp[k] = v
		}
		out.Choices[q] = cp
	}
	for q, m := range d.Confidence {
		cp := make(map[string]int64, len(m))
		for k, v := range m {
			cp[k] = v
		}
		out.Confidence[q] = cp
	}
	return out
}

// cloneDecisions deep-copies a whole per-model distribution map, so the
// writer owns its snapshot and a caller reusing its map cannot corrupt the
// next interval's delta.
func cloneDecisions(in map[string]DecisionCounts) map[string]DecisionCounts {
	out := make(map[string]DecisionCounts, len(in))
	for model, d := range in {
		out[model] = cloneDecisionCounts(d)
	}
	return out
}
