package llm

import (
	"strings"
	"time"
)

// Rate is USD per 1M tokens at off-peak time.
type Rate struct {
	CacheHit  float64
	CacheMiss float64
	Output    float64
}

// Pricing maps a model id prefix to its off-peak rate.
type Pricing map[string]Rate

// DefaultPricing is DeepSeek's published off-peak pricing (2026-11).
// Peak hours cost 2x; see PeakMultiplier.
var DefaultPricing = Pricing{
	ModelFlash: {CacheHit: 0.003, CacheMiss: 0.15, Output: 0.6},
	ModelPro:   {CacheHit: 0.022, CacheMiss: 0.66, Output: 1.98},
}

// PeakMultiplier returns 2 during DeepSeek peak hours (01:00-04:00 and
// 06:00-10:00 UTC, Monday-Friday) and 1 otherwise. Chinese public holidays
// (off-peak) are not modelled, so estimates err on the high side.
func PeakMultiplier(t time.Time) float64 {
	u := t.UTC()
	if u.Weekday() == time.Saturday || u.Weekday() == time.Sunday {
		return 1
	}
	h := u.Hour()
	if (h >= 1 && h < 4) || (h >= 6 && h < 10) {
		return 2
	}
	return 1
}

// Cost estimates the USD cost of one call. Unknown models cost 0.
func (p Pricing) Cost(model string, u Usage, at time.Time) float64 {
	rate, ok := p.lookup(model)
	if !ok {
		return 0
	}
	hit, miss := u.PromptCacheHitTokens, u.PromptCacheMissTokens
	if hit+miss == 0 {
		miss = u.PromptTokens
	}
	usd := (float64(hit)*rate.CacheHit + float64(miss)*rate.CacheMiss + float64(u.CompletionTokens)*rate.Output) / 1e6
	return usd * PeakMultiplier(at)
}

func (p Pricing) lookup(model string) (Rate, bool) {
	if r, ok := p[model]; ok {
		return r, true
	}
	// Response model ids may carry a version suffix.
	best, bestLen := Rate{}, 0
	for id, r := range p {
		if strings.HasPrefix(model, id) && len(id) > bestLen {
			best, bestLen = r, len(id)
		}
	}
	return best, bestLen > 0
}
