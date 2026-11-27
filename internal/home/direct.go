package home

import (
	"context"
	"encoding/json/v2"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// ivkaiv: direct path for "включи/выключи <имя>" and "сделай/поставь <климат> <число>".
// When the phrase unambiguously names one controllable device in HA, the command
// goes straight to HA: no full inventory (~2.8 s) and no LLM planner (~1-2 s).
// The same in-memory index also powers device-aware routing (HomeHint) so bare
// song titles like "включи Батарейку" never get mistaken for smart-home devices.

var directDomains = []string{"light", "switch", "fan", "media_player"}
var indexDomains = []string{"light", "switch", "fan", "media_player", "climate", "cover", "vacuum"}

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

// setpointCommand parses commands like "сделай колонку 41", "поставь колонку на 42 градуса",
// "сделай температуру в колонке 41", "поставь котёл 21.5".
// Relative changes ("теплее на 2 градуса") return ok=false so the planner handles them.
func setpointCommand(text string) (target string, temp float64, ok bool) {
	s := strings.ReplaceAll(strings.ToLower(text), "ё", "е")
	raw := strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '.' && r != ','
	})
	for i := range raw {
		raw[i] = strings.Trim(raw[i], ".,")
	}
	raw = slices.DeleteFunc(raw, func(w string) bool { return w == "" })
	for len(raw) > 0 && (raw[0] == "алиса" || raw[0] == "пожалуйста") {
		raw = raw[1:]
	}
	for len(raw) > 0 && raw[len(raw)-1] == "пожалуйста" {
		raw = raw[:len(raw)-1]
	}
	if len(raw) < 3 {
		return "", 0, false
	}
	switch raw[0] {
	case "сделай", "поставь", "установи", "выстави", "задай":
	default:
		return "", 0, false
	}
	rest := raw[1:]
	for _, w := range rest {
		switch w {
		case "теплее", "холоднее", "выше", "ниже", "больше", "меньше", "плюс", "минус", "прибавь", "убавь":
			return "", 0, false
		}
	}
	numIdx := -1
	var parsed float64
	for i, w := range rest {
		clean := strings.Trim(w, ".,")
		if clean == "" {
			continue
		}
		v, err := strconv.ParseFloat(strings.ReplaceAll(clean, ",", "."), 64)
		if err == nil {
			if numIdx >= 0 {
				return "", 0, false // multiple numbers -> ambiguous
			}
			numIdx = i
			parsed = v
		}
	}
	if numIdx < 0 || parsed < 5 || parsed > 85 {
		return "", 0, false
	}
	var nameWords []string
	for i, w := range rest {
		if i == numIdx {
			continue
		}
		nw := normName(w)
		if nw == "" {
			continue
		}
		// Skip filler/unit words around the target temperature.
		if nw == "на" || nw == "до" || nw == "в" || nw == "во" || nw == "температур" ||
			strings.HasPrefix(nw, "градус") || nw == "цельси" || nw == "c" || nw == "с" {
			continue
		}
		nameWords = append(nameWords, nw)
	}
	target = strings.Join(nameWords, " ")
	if target == "" {
		return "", 0, false
	}
	return target, parsed, true
}

func matchSetpointEntity(text string, entities []Entity) (Plan, Entity, bool) {
	target, temp, ok := setpointCommand(text)
	if !ok {
		return Plan{}, Entity{}, false
	}
	targetKey := roomKey(target)
	var exact []Entity
	seen := map[string]bool{}
	for _, e := range entities {
		if !strings.HasPrefix(e.ID, "climate.") || e.State == "unavailable" || e.State == "unknown" || seen[e.ID] {
			continue
		}
		for _, n := range append([]string{e.Name}, e.Aliases...) {
			if normName(n) == target || (targetKey != "" && roomKey(n) == targetKey) {
				seen[e.ID] = true
				exact = append(exact, e)
				break
			}
		}
	}
	if len(exact) == 1 {
		e := exact[0]
		return Plan{Actions: []Action{{Domain: "climate", Service: "set_temperature", EntityID: e.ID, Data: map[string]any{"temperature": temp}}}}, e, true
	}
	// Fallback for "колонк"/"газов колонк"/"водонагревател" even if HA aliases were cleared:
	// unambiguously prefer the single DHW / hot-water climate entity.
	if len(exact) == 0 && (targetKey == "колонк" || targetKey == "газов колонк" || targetKey == "водонагревател" || targetKey == "вод" || targetKey == "горяч вод") {
		var dhw []Entity
		for _, e := range entities {
			if !strings.HasPrefix(e.ID, "climate.") || e.State == "unavailable" || e.State == "unknown" {
				continue
			}
			hay := strings.ToLower(e.ID + " " + e.Name + " " + strings.Join(e.Aliases, " "))
			if strings.Contains(hay, "dhw") || strings.Contains(hay, "горяч") || strings.Contains(hay, "колонк") || strings.Contains(hay, "водонагрев") {
				dhw = append(dhw, e)
			}
		}
		if len(dhw) == 1 {
			e := dhw[0]
			return Plan{Actions: []Action{{Domain: "climate", Service: "set_temperature", EntityID: e.ID, Data: map[string]any{"temperature": temp}}}}, e, true
		}
	}
	return Plan{}, Entity{}, false
}

func (c *Controller) directSetpointPlan(text string) (Plan, Entity, bool) {
	entities, ok := c.names.allEntities()
	if !ok {
		return Plan{}, Entity{}, false
	}
	return matchSetpointEntity(text, entities)
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
		var direct []Entity
		for _, m := range match {
			domain, _, _ := strings.Cut(m.ID, ".")
			if slices.Contains(directDomains, domain) {
				direct = append(direct, m)
			}
		}
		if len(direct) != 1 {
			return Plan{}, Entity{}, false
		}
		e = direct[0]
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

// genericHomeWords contains whole-word normalized (normName) smart-home nouns.
// Whole-word comparison prevents false matches against song titles:
// "батарейка" -> "батарейк" != "батаре"; "Светлана" -> "светлан" != "свет".
var genericHomeWords = map[string]bool{
	"свет": true, "освещени": true, "ламп": true, "лампочк": true, "люстр": true,
	"подсветк": true, "светильник": true, "ночник": true, "бра": true, "торшер": true,
	"лент": true, "гирлянд": true, "розетк": true, "выключател": true, "реле": true,
	"рубильник": true, "штор": true, "жалюз": true, "карниз": true,
	"кондиционер": true, "термостат": true, "обогревател": true, "обогрев": true,
	"батаре": true, "радиатор": true, "вентилятор": true, "вытяжк": true,
	"увлажнител": true, "очистител": true, "пылесос": true,
	"телевизор": true, "приставк": true, "проектор": true, "тв": true, "tv": true,
	"колонк": true, "котел": true, "котл": true, "отоплени": true,
	"водонагревател": true, "бойлер": true, "горяч": true,
	"датчик": true, "температур": true, "влажност": true, "климат": true, "трек": true,
}

// HomeHint checks whether text references any real controllable HA device,
// alias, room, or generic smart-home device noun, and returns a compact
// summary of known rooms and devices for the LLM router classifier.
func (c *Controller) HomeHint(text string) (known bool, matches bool, summary string) {
	entities, areas, summary, ok := c.names.snapshot()
	if !ok {
		return false, false, ""
	}
	normWords := strings.Fields(normName(text))
	for _, w := range normWords {
		if genericHomeWords[w] {
			return true, true, summary
		}
	}
	rWords := roomWords(text)
	for _, area := range areas {
		if aw := roomWords(area); len(aw) > 0 && wordSequence(rWords, aw) {
			return true, true, summary
		}
	}
	for _, e := range entities {
		for _, n := range append([]string{e.Name}, e.Aliases...) {
			nw := strings.Fields(normName(n))
			if len(nw) == 0 || (len(nw) == 1 && len([]rune(nw[0])) < 2) {
				continue
			}
			if wordSequence(normWords, nw) {
				return true, true, summary
			}
		}
	}
	return true, false, summary
}

// nameIndex keeps the controllable HA devices by normalized name and alias, so a
// hit needs no ha-mcp search (~1 s). RefreshNames rebuilds it in the background
// (main.go, every NameIndexTTL); an index older than 3×TTL is ignored and the
// live search above is used, as before the first build.
const NameIndexTTL = 5 * time.Minute

type nameIndex struct {
	mu       sync.Mutex
	byName   map[string][]Entity
	entities []Entity
	areaList []string
	summary  string
	built    time.Time
}

func (x *nameIndex) lookup(name string) ([]Entity, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.byName == nil || time.Since(x.built) > 3*NameIndexTTL {
		return nil, false
	}
	return x.byName[name], true
}

func (x *nameIndex) allEntities() ([]Entity, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.byName == nil || time.Since(x.built) > 3*NameIndexTTL {
		return nil, false
	}
	return x.entities, true
}

func (x *nameIndex) snapshot() (entities []Entity, areas []string, summary string, ok bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.byName == nil || time.Since(x.built) > 3*NameIndexTTL {
		return nil, nil, "", false
	}
	return x.entities, x.areaList, x.summary, true
}

// Room names are hints for recognizing explicit rooms, never permission to use stale entities.
func (x *nameIndex) areas() []string {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.byName == nil || time.Since(x.built) > 3*NameIndexTTL {
		return nil
	}
	return slices.Clone(x.areaList)
}

func buildCatalogSummary(entities []Entity, areas []string) string {
	const maxRunes = 900
	var b strings.Builder
	runeCount := 0
	appendStr := func(s string) bool {
		s = strings.ToValidUTF8(s, "")
		n := len([]rune(s))
		if runeCount+n > maxRunes {
			return false
		}
		b.WriteString(s)
		runeCount += n
		return true
	}
	if len(areas) > 0 {
		if appendStr("Комнаты: ") {
			for i, a := range areas {
				sep := ""
				if i > 0 {
					sep = ", "
				}
				if !appendStr(sep + a) {
					break
				}
			}
			appendStr(". ")
		}
	}
	var devs []string
	seen := map[string]bool{}
	for _, e := range entities {
		name := strings.TrimSpace(e.Name)
		if name == "" && len(e.Aliases) == 0 {
			continue
		}
		label := name
		if len(e.Aliases) > 0 {
			if label != "" {
				label += " (" + strings.Join(e.Aliases, ", ") + ")"
			} else {
				label = strings.Join(e.Aliases, ", ")
			}
		}
		if !seen[label] {
			seen[label] = true
			devs = append(devs, label)
		}
	}
	if len(devs) > 0 && appendStr("Устройства: ") {
		for i, d := range devs {
			sep := ""
			if i > 0 {
				sep = ", "
			}
			if !appendStr(sep + d) {
				break
			}
		}
		appendStr(".")
	}
	return strings.TrimSpace(b.String())
}

// RefreshNames rereads names and aliases of controllable smart-home devices.
func (c *Controller) RefreshNames(ctx context.Context) error {
	entities, err := (&Controller{Tools: c.Tools, Domains: indexDomains}).Entities(ctx)
	if err != nil {
		return err // keep the old index
	}
	c.setNameIndex(entities)
	return nil
}

func (c *Controller) setNameIndex(entities []Entity) {
	byName := map[string][]Entity{}
	var areas []string
	seenArea := map[string]bool{}
	for _, e := range entities {
		if a := strings.TrimSpace(e.Area); a != "" && !seenArea[a] {
			seenArea[a] = true
			areas = append(areas, a)
		}
		seen := map[string]bool{}
		for _, n := range append([]string{e.Name}, e.Aliases...) {
			if k := normName(n); k != "" && !seen[k] {
				seen[k] = true
				byName[k] = append(byName[k], e)
			}
		}
	}
	slices.Sort(areas)
	copied := slices.Clone(entities)
	summary := buildCatalogSummary(copied, areas)
	c.names.mu.Lock()
	defer c.names.mu.Unlock()
	c.names.byName = byName
	c.names.entities = copied
	c.names.areaList = areas
	c.names.summary = summary
	c.names.built = time.Now()
}
