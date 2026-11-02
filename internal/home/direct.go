package home

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
)

// NameIndexTTL is the recommended refresh interval for the name cache.
const NameIndexTTL = 5 * time.Minute

type nameIndex struct {
	mu     sync.Mutex
	byName map[string][]Entity
	built  time.Time
}

func (x *nameIndex) lookup(name string) ([]Entity, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.byName == nil || time.Since(x.built) >= NameIndexTTL {
		return nil, false
	}
	return slices.Clone(x.byName[name]), true
}

// normName permits common spoken Russian case endings, preserving every
// collision in the index. A normalized name is never permission to act.
func normName(s string) string {
	s = strings.ReplaceAll(strings.ToLower(s), "ё", "е")
	words := strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	for i, word := range words {
		r := []rune(word)
		if len(r) >= 4 && strings.ContainsRune("аяуюыиеоьй", r[len(r)-1]) {
			words[i] = string(r[:len(r)-1])
		}
	}
	return strings.Join(words, " ")
}

func directCommand(text string) (service, name string, ok bool) {
	words := strings.Fields(strings.TrimSpace(strings.ToLower(text)))
	if len(words) < 2 {
		return "", "", false
	}
	service = map[string]string{"включи": "turn_on", "выключи": "turn_off"}[words[0]]
	name = normName(strings.Join(words[1:], " "))
	return service, name, service != "" && name != ""
}

// RefreshNames atomically replaces the name cache only after complete inventory.
// Include every configured domain, including noncontrollable ones, so a shared
// alias cannot silently select a controllable device over another entity.
// A failed refresh leaves the previous cache intact until its hard expiry.
func (c *Controller) RefreshNames(ctx context.Context) error {
	entities, err := c.Entities(ctx)
	if err != nil {
		return err
	}
	byName := map[string][]Entity{}
	for _, e := range entities {
		seen := map[string]bool{}
		for _, name := range append([]string{e.Name}, e.Aliases...) {
			key := normName(name)
			if key != "" && !seen[key] {
				seen[key] = true
				byName[key] = append(byName[key], e)
			}
		}
	}
	c.names.mu.Lock()
	defer c.names.mu.Unlock()
	c.names.byName, c.names.built = byName, time.Now()
	return nil
}

func (c *Controller) directPlan(ctx context.Context, text string) (Plan, Entity, bool, error) {
	service, name, ok := directCommand(text)
	if !ok {
		return Plan{}, Entity{}, false, nil
	}
	matches, ready := c.names.lookup(name)
	if !ready {
		// Never infer uniqueness from a truncated search or a failed domain.
		// Discovery failures fall back to the regular inventory/planner path.
		if err := c.RefreshNames(ctx); err != nil {
			if ctx.Err() != nil {
				return Plan{}, Entity{}, false, ctx.Err()
			}
			return Plan{}, Entity{}, false, nil
		}
		matches, _ = c.names.lookup(name)
	}
	if len(matches) != 1 {
		return Plan{}, Entity{}, false, nil
	}
	e := matches[0]
	domain, _, _ := strings.Cut(e.ID, ".")
	if !slices.Contains([]string{"light", "switch", "fan", "media_player"}, domain) {
		return Plan{}, Entity{}, false, nil
	}
	if !validEntityID(e.ID) || e.State == "unavailable" || e.State == "unknown" {
		return Plan{}, Entity{}, false, nil
	}
	if _, ok := serviceFields[domain+"."+service]; !ok {
		return Plan{}, Entity{}, false, nil
	}
	return Plan{Actions: []Action{{Domain: domain, Service: service, EntityID: e.ID, Data: map[string]any{}}}}, e, true, nil
}
