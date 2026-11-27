package home

import (
	"context"
	"encoding/json/v2"
	"strings"
	"sync"
	"time"
	"unicode"
)

// ivkaiv: direct path for "включи/выключи <имя>". When the rest of the phrase is
// exactly the name or an alias of one device, the command goes straight to HA:
// no full inventory (~2.8 s) and no planner (~0.5 s). Anything else — rooms,
// numbers, "всё", two matches, a near miss — falls back to the planner path.

var directDomains = []string{"light", "switch", "fan", "media_player"}

// directCommand splits "Включи трек." into ("turn_on", "трек").
func directCommand(text string) (service, name string, ok bool) {
	verb, rest, _ := strings.Cut(strings.TrimSpace(strings.ToLower(text)), " ")
	svc := map[string]string{"включи": "turn_on", "выключи": "turn_off"}[verb]
	if name := normName(rest); svc != "" && name != "" {
		return svc, name, true
	}
	return "", "", false
}

// normName lowercases, folds ё, keeps only letters and digits separated by single
// spaces and drops one final vowel (or ь/й) of words of 4+ letters, so a spoken
// case form meets the name in HA: "подсветку над столом" = "Подсветка над столом",
// "лампу" = "лампа", "на кухне" = "на кухня". Names that collide after this give
// two matches, and two matches are never a direct command.
func normName(s string) string {
	s = strings.ReplaceAll(strings.ToLower(s), "ё", "е")
	words := strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	for i, w := range words {
		if r := []rune(w); len(r) >= 4 && strings.ContainsRune("аяуюыиеоьй", r[len(r)-1]) {
			words[i] = string(r[:len(r)-1])
		}
	}
	return strings.Join(words, " ")
}

// directPlan finds the single device the phrase names; ok=false means "not a
// direct command" and the caller takes the planner path.
// The in-memory index is consulted for every phrase: it holds only full device
// names, so "подсветка над столом" hits while "свет на кухне" (no such name)
// misses for free. The slow live search runs only without an index and only
// for phrases with no generic device words (live=true).
func (c *Controller) directPlan(ctx context.Context, text string, live bool) (plan Plan, e Entity, ok bool) {
	service, name, ok := directCommand(text)
	if !ok {
		return Plan{}, Entity{}, false
	}
	if match, ok := c.names.lookup(name); ok {
		if len(match) != 1 {
			return Plan{}, Entity{}, false
		}
		e = match[0]
		domain, _, _ := strings.Cut(e.ID, ".")
		return Plan{Actions: []Action{{Domain: domain, Service: service, EntityID: e.ID, Data: map[string]any{}}}}, e, true
	}
	if !live {
		return Plan{}, Entity{}, false
	}
	found := make([][]Entity, len(directDomains))
	var wg sync.WaitGroup
	for i, domain := range directDomains {
		wg.Go(func() {
			raw, err := c.Tools.Call(ctx, "ha_search", map[string]any{"query": name, "domain_filter": domain, "limit": 20, "include_hidden": false, "result_fields": []string{"entity_id", "friendly_name", "state", "area", "aliases", "member_entity_ids"}})
			if err != nil {
				return // a failed lookup only means no shortcut
			}
			var result struct {
				Entities []Entity `json:"entities"`
			}
			if json.Unmarshal(raw, &result) == nil {
				found[i] = result.Entities
			}
		})
	}
	wg.Wait()
	var match []Entity
	seen := map[string]bool{}
	for _, list := range found {
		for _, e := range list {
			if seen[e.ID] {
				continue
			}
			for _, n := range append([]string{e.Name}, e.Aliases...) {
				if normName(n) == name {
					seen[e.ID] = true
					match = append(match, e)
					break
				}
			}
		}
	}
	if len(match) != 1 {
		return Plan{}, Entity{}, false
	}
	e = match[0]
	domain, _, _ := strings.Cut(e.ID, ".")
	return Plan{Actions: []Action{{Domain: domain, Service: service, EntityID: e.ID, Data: map[string]any{}}}}, e, true
}

// nameIndex keeps the direct-command devices by normalized name and alias, so a
// hit needs no ha-mcp search (~1 s). RefreshNames rebuilds it in the background
// (main.go, every NameIndexTTL); an index older than 3×TTL is ignored and the
// live search above is used, as before the first build.
const NameIndexTTL = 5 * time.Minute

type nameIndex struct {
	mu     sync.Mutex
	byName map[string][]Entity
	built  time.Time
}

func (x *nameIndex) lookup(name string) ([]Entity, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.byName == nil || time.Since(x.built) > 3*NameIndexTTL {
		return nil, false
	}
	return x.byName[name], true
}

// Room names are hints for recognizing explicit rooms, never permission to use stale entities.
func (x *nameIndex) areas() []string {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.byName == nil || time.Since(x.built) > 3*NameIndexTTL {
		return nil
	}
	areas := []string{}
	seen := map[string]bool{}
	for _, entities := range x.byName {
		for _, e := range entities {
			if e.Area != "" && !seen[e.Area] {
				seen[e.Area] = true
				areas = append(areas, e.Area)
			}
		}
	}
	return areas
}

// RefreshNames rereads names and aliases of the direct-command devices.
func (c *Controller) RefreshNames(ctx context.Context) error {
	entities, err := (&Controller{Tools: c.Tools, Domains: directDomains}).Entities(ctx)
	if err != nil {
		return err // keep the old index
	}
	byName := map[string][]Entity{}
	for _, e := range entities {
		seen := map[string]bool{}
		for _, n := range append([]string{e.Name}, e.Aliases...) {
			if k := normName(n); k != "" && !seen[k] {
				seen[k] = true
				byName[k] = append(byName[k], e)
			}
		}
	}
	c.names.mu.Lock()
	defer c.names.mu.Unlock()
	c.names.byName, c.names.built = byName, time.Now()
	return nil
}
