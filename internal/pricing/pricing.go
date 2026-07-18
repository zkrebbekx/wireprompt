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
type ModelPrice struct {
	Match      string  `json:"match"`
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
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
func (t *Table) Lookup(model string) (ModelPrice, bool) {
	for _, m := range t.Models {
		if strings.HasPrefix(model, m.Match) {
			return m, true
		}
	}
	return ModelPrice{}, false
}

// Cost computes the USD cost for one request. Unknown models cost 0 — callers
// can detect that via the ok return of Lookup if they need to surface it.
func (t *Table) Cost(model string, input, output, cacheRead, cacheWrite int64) float64 {
	p, ok := t.Lookup(model)
	if !ok {
		return 0
	}
	const mtok = 1_000_000
	return float64(input)/mtok*p.Input +
		float64(output)/mtok*p.Output +
		float64(cacheRead)/mtok*p.CacheRead +
		float64(cacheWrite)/mtok*p.CacheWrite
}
