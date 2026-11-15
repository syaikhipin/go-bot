package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

// UsageSnapshot separates durable request checkpoints from provider-reported
// outcomes. A checkpoint can precede a crash without a provider call, so it
// must not be presented as an exact upstream-call count. Missing usage samples
// remain visible in UnmeteredOutcomes. This is not Credits wallet state.
type UsageSnapshot struct {
	MainRequestCheckpoints uint64 `json:"mainRequestCheckpoints"`
	MainSucceeded          uint64 `json:"mainSucceeded"`
	MainFailed             uint64 `json:"mainFailed"`
	MainUnsettled          uint64 `json:"mainUnsettled"`
	SummarySucceeded       uint64 `json:"summarySucceeded"`
	SummaryFailed          uint64 `json:"summaryFailed"`
	ReportedSamples        uint64 `json:"reportedSamples"`
	UnmeteredOutcomes      uint64 `json:"unmeteredOutcomes"`
	InputTokens            int64  `json:"inputTokens"`
	OutputTokens           int64  `json:"outputTokens"`
	TotalTokens            int64  `json:"totalTokens"`
	CachedInputTokens      int64  `json:"cachedInputTokens"`
	CacheCreationTokens    int64  `json:"cacheCreationTokens"`
}

type usageProjection struct {
	processed int
	result    UsageSnapshot
	pending   map[string]attemptLocation
	seen      map[string]bool
	err       error
}

type attemptLocation struct {
	turn uint64
	step uint64
}

// UsageAccumulator folds a validated journal stream without retaining event
// bodies. Historical stores validate identity and sequence while scanning.
type UsageAccumulator struct{ projection usageProjection }

func (a *UsageAccumulator) Visit(event Event) error {
	if a.projection.err != nil {
		return a.projection.err
	}
	if err := a.projection.apply(event); err != nil {
		a.projection.err = err
		return err
	}
	return nil
}

func (a *UsageAccumulator) Snapshot() (UsageSnapshot, error) {
	if a.projection.err != nil {
		return UsageSnapshot{}, a.projection.err
	}
	result := a.projection.result
	result.MainUnsettled = uint64(len(a.projection.pending))
	return result, nil
}

func (p *usageProjection) apply(event Event) error {
	switch event.Type {
	case "model/request":
		var record struct {
			AttemptID string `json:"attemptId"`
		}
		if err := json.Unmarshal(event.Data, &record); err != nil {
			return fmt.Errorf("decode model request %d: %w", event.Seq, err)
		}
		p.result.MainRequestCheckpoints++
		if record.AttemptID != "" {
			if p.pending == nil {
				p.pending = make(map[string]attemptLocation)
				p.seen = make(map[string]bool)
			}
			if p.seen[record.AttemptID] {
				return fmt.Errorf("duplicate model attempt ID at event %d", event.Seq)
			}
			p.seen[record.AttemptID] = true
			p.pending[record.AttemptID] = attemptLocation{turn: event.Turn, step: event.Step}
		}
	case "assistant/message":
		var record AssistantMessage
		if err := json.Unmarshal(event.Data, &record); err != nil {
			return fmt.Errorf("decode assistant usage %d: %w", event.Seq, err)
		}
		if record.AttemptID != "" {
			location, ok := p.pending[record.AttemptID]
			if !ok {
				return fmt.Errorf("assistant event %d has no matching model request", event.Seq)
			}
			if location.turn != event.Turn || location.step != event.Step {
				return fmt.Errorf("assistant event %d settles a model request from a different turn or step", event.Seq)
			}
		}
		p.result.MainSucceeded++
		delete(p.pending, record.AttemptID)
		return p.addUsage(event.Seq, record.Usage)
	case "model/failure":
		var record struct {
			AttemptID string      `json:"attemptId"`
			Usage     *ModelUsage `json:"usage"`
		}
		if err := json.Unmarshal(event.Data, &record); err != nil {
			return fmt.Errorf("decode model failure usage %d: %w", event.Seq, err)
		}
		if record.AttemptID != "" {
			location, ok := p.pending[record.AttemptID]
			if !ok {
				return fmt.Errorf("model failure %d has no matching request", event.Seq)
			}
			if location.turn != event.Turn || location.step != event.Step {
				return fmt.Errorf("model failure %d settles a request from a different turn or step", event.Seq)
			}
		}
		p.result.MainFailed++
		delete(p.pending, record.AttemptID)
		return p.addUsage(event.Seq, record.Usage)
	case "compaction/summary":
		var record struct {
			Usage *ModelUsage `json:"usage"`
		}
		if err := json.Unmarshal(event.Data, &record); err != nil {
			return fmt.Errorf("decode summary usage %d: %w", event.Seq, err)
		}
		p.result.SummarySucceeded++
		return p.addUsage(event.Seq, record.Usage)
	case "compaction/end":
		var record struct {
			Status string      `json:"status"`
			Usage  *ModelUsage `json:"usage"`
		}
		if err := json.Unmarshal(event.Data, &record); err != nil {
			return fmt.Errorf("decode compaction end usage %d: %w", event.Seq, err)
		}
		if record.Status == "failed" {
			p.result.SummaryFailed++
			return p.addUsage(event.Seq, record.Usage)
		}
		if record.Usage != nil {
			return fmt.Errorf("non-failed compaction end %d contains duplicate usage", event.Seq)
		}
	}
	return nil
}

func (p *usageProjection) addUsage(seq uint64, usage *ModelUsage) error {
	if usage == nil {
		p.result.UnmeteredOutcomes++
		return nil
	}
	values := []*int64{&p.result.InputTokens, &p.result.OutputTokens, &p.result.TotalTokens, &p.result.CachedInputTokens, &p.result.CacheCreationTokens}
	deltas := []int64{usage.InputTokens, usage.OutputTokens, usage.TotalTokens, usage.CachedInputTokens, usage.CacheCreationInputTokens}
	for index, delta := range deltas {
		if delta < 0 || *values[index] > math.MaxInt64-delta {
			return fmt.Errorf("invalid or overflowing provider token usage at event %d", seq)
		}
	}
	for index, delta := range deltas {
		*values[index] += delta
	}
	p.result.ReportedSamples++
	return nil
}

func (p *usageProjection) snapshot(events []Event) (UsageSnapshot, error) {
	if p.err != nil {
		return UsageSnapshot{}, p.err
	}
	if p.processed > len(events) {
		return UsageSnapshot{}, errors.New("usage projection cursor exceeds durable Session prefix")
	}
	for p.processed < len(events) {
		if err := p.apply(events[p.processed]); err != nil {
			p.err = err
			return UsageSnapshot{}, err
		}
		p.processed++
	}
	result := p.result
	result.MainUnsettled = uint64(len(p.pending))
	return result, nil
}

// ProjectUsage reconstructs the same meter from an immutable durable history.
func ProjectUsage(events []Event) (UsageSnapshot, error) {
	var projection usageProjection
	return projection.snapshot(events)
}

// DurableUsage extends the cached projection only through the flushed prefix.
func (s *Session) DurableUsage() (UsageSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage.snapshot(s.events[:s.durable])
}
