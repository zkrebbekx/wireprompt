// Package pricing maps model identifiers to per-token prices and computes
// request costs. The embedded table ships with the binary; users can override
// or extend it with ~/.config/wireprompt/pricing.json (same schema).
package pricing

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed pricing.json
var embedded []byte

// ModelPrice holds USD prices per million tokens for one model prefix.
// CacheWrite is the 5-minute-TTL cache-creation rate; CacheWrite1h the
// 1-hour-TTL rate. A zero CacheWrite1h falls back to 1.6x CacheWrite (the
// 2x-vs-1.25x input-rate ratio Anthropic bills).
type ModelPrice struct {
	Match        string  `json:"match"`
	Input        float64 `json:"input"`
	Output       float64 `json:"output"`
	CacheRead    float64 `json:"cache_read"`
	CacheWrite   float64 `json:"cache_write"`
	CacheWrite1h float64 `json:"cache_write_1h"`
}

func (p ModelPrice) cacheWrite1h() float64 {
	if p.CacheWrite1h > 0 {
		return p.CacheWrite1h
	}
	return p.CacheWrite * 1.6
}

// Table resolves model ids to prices via longest-prefix match.
type Table struct {
	Updated string       `json:"updated"`
	Models  []ModelPrice `json:"models"`
}

type tableFile struct {
	Updated string       `json:"updated"`
	Models  []ModelPrice `json:"models"`
}

// Load returns the embedded table merged with the user override file, if one
// exists. Override entries with the same Match replace embedded entries.
func Load() (*Table, error) {
	t, err := parse(embedded)
	if err != nil {
		return nil, fmt.Errorf("embedded pricing table: %w", err)
	}
	if home, herr := os.UserHomeDir(); herr == nil {
		p := filepath.Join(home, ".config", "wireprompt", "pricing.json")
		if data, rerr := os.ReadFile(p); rerr == nil {
			override, perr := parse(data)
			if perr != nil {
				return nil, fmt.Errorf("override pricing table %s: %w", p, perr)
			}
			t.merge(override)
		}
	}
	return t, nil
}

func parse(data []byte) (*Table, error) {
	var f tableFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	t := &Table{Updated: f.Updated, Models: f.Models}
	// Longest prefix first so the first match found is the most specific.
	sort.SliceStable(t.Models, func(i, j int) bool {
		return len(t.Models[i].Match) > len(t.Models[j].Match)
	})
	return t, nil
}

func (t *Table) merge(o *Table) {
	byMatch := make(map[string]int, len(t.Models))
	for i, m := range t.Models {
		byMatch[m.Match] = i
	}
	for _, m := range o.Models {
		if i, ok := byMatch[m.Match]; ok {
			t.Models[i] = m
		} else {
			t.Models = append(t.Models, m)
		}
	}
	if o.Updated != "" {
		t.Updated = o.Updated
	}
	sort.SliceStable(t.Models, func(i, j int) bool {
		return len(t.Models[i].Match) > len(t.Models[j].Match)
	})
}

// Lookup returns the price entry for a model id, or false when unknown.
// Vendor-namespaced ids (OpenRouter's "anthropic/claude-sonnet-5") fall back
// to matching the segment after the slash.
func (t *Table) Lookup(model string) (ModelPrice, bool) {
	for _, m := range t.Models {
		if strings.HasPrefix(model, m.Match) {
			return m, true
		}
	}
	if _, after, ok := strings.Cut(model, "/"); ok {
		for _, m := range t.Models {
			if strings.HasPrefix(after, m.Match) {
				return m, true
			}
		}
	}
	return ModelPrice{}, false
}

// Cost computes the USD cost for one request. The ok return is false for
// unknown models (cost 0) so callers can surface unpriced records.
func (t *Table) Cost(model string, input, output, cacheRead, cacheWrite5m, cacheWrite1h int64) (float64, bool) {
	p, ok := t.Lookup(model)
	if !ok {
		return 0, false
	}
	const mtok = 1_000_000
	return float64(input)/mtok*p.Input +
		float64(output)/mtok*p.Output +
		float64(cacheRead)/mtok*p.CacheRead +
		float64(cacheWrite5m)/mtok*p.CacheWrite +
		float64(cacheWrite1h)/mtok*p.cacheWrite1h(), true
}

// Saved computes how many dollars caching saved on a request: what the
// cache-read tokens would have cost at the full input rate, minus what they
// actually cost at the cache-read rate.
func (t *Table) Saved(model string, cacheRead int64) float64 {
	p, ok := t.Lookup(model)
	if !ok {
		return 0
	}
	const mtok = 1_000_000
	saved := float64(cacheRead) / mtok * (p.Input - p.CacheRead)
	if saved < 0 {
		return 0
	}
	return saved
}
