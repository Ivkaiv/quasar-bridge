package home

import (
	"encoding/json/v2"
	"slices"
	"strings"
	"unicode"
)

// areaSelection is a hard inventory boundary, not merely a planner hint.
type areaSelection struct {
	entities []Entity
	areas    []string
	catalog  []string
	source   string
	answer   string
}

func (s areaSelection) context() string {
	if s.source == "" && len(s.areas) == 0 {
		return ""
	}
	data, err := json.Marshal(struct {
		Areas  []string `json:"areas"`
		Source string   `json:"source"`
	}{s.areas, s.source})
	if err != nil {
		return ""
	}
	return "\nКонтекст комнат (используй только переданный список устройств): " + string(data)
}

func (c *Controller) selectArea(text, request string, entities []Entity) areaSelection {
	var catalog []string
	add := func(area string) {
		area = strings.TrimSpace(area)
		if area == "" {
			return
		}
		for _, known := range catalog {
			if sameArea(known, area) {
				return
			}
		}
		catalog = append(catalog, area)
	}
	for _, area := range c.names.areas() {
		add(area)
	}
	for _, e := range entities {
		add(e.Area)
	}
	add(c.DefaultArea)

	current, unknown, excluded := mentionedAreas(text, catalog, entities...)
	if ambiguousAreaNames(current) {
		return areaSelection{source: "clarification", answer: "Названия комнат похожи. Уточните полное название нужной комнаты?"}
	}
	if excluded {
		return areaSelection{source: "clarification", answer: "Назовите нужную комнату без исключений — где выполнить команду?"}
	}
	if unknown {
		return areaSelection{source: "clarification", answer: "Не удалось определить указанную комнату. Как называется комната в Home Assistant?"}
	}
	if wholeHome(text) {
		return areaSelection{entities: entities, source: "whole_home"}
	}
	selection := areaSelection{entities: entities, areas: current, catalog: catalog, source: "explicit"}
	if len(current) == 0 {
		previous, unknown, excluded := mentionedAreas(request, catalog, entities...)
		if ambiguousAreaNames(previous) {
			return areaSelection{source: "clarification", answer: "Названия комнат похожи. Уточните полное название нужной комнаты?"}
		}
		if unknown || excluded {
			return areaSelection{source: "clarification", answer: "В какой комнате выполнить команду?"}
		}
		if wholeHome(request) {
			return areaSelection{entities: entities, source: "whole_home"}
		}
		selection.areas, selection.source = previous, "request"
		if len(previous) == 0 {
			// An explicitly named boiler/water heater remains addressable from
			// every speaker. A generic temperature question is still room scoped.
			if named := namedClimate(text+" "+request, entities); len(named) != 0 {
				return areaSelection{entities: named, source: "named_climate"}
			}
			if strings.TrimSpace(c.DefaultArea) == "" {
				selection.source = "global"
				return selection
			}
			selection.areas, selection.source = []string{strings.TrimSpace(c.DefaultArea)}, "default"
		}
	}
	selection.entities = nil
	for _, e := range entities {
		if entityInAreas(e, selection.areas, catalog) {
			selection.entities = append(selection.entities, e)
		}
	}
	if len(selection.entities) == 0 {
		selection.answer = "В указанной комнате не нашлось подходящих устройств. Уточните комнату или название устройства?"
	}
	return selection
}

// roomWords compares whole words while folding common Russian room inflections.
// It deliberately does not use substring matching ("зал" must not match "вокзал").
func roomWords(text string) []string {
	words := strings.FieldsFunc(strings.ReplaceAll(strings.ToLower(text), "ё", "е"), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '%'
	})
	for i, word := range words {
		runes := []rune(word)
		for _, suffix := range []string{"ая", "яя", "ой", "ей", "ую", "юю", "ия", "ью", "ям", "ях", "ом", "ем", "ов", "ам", "ах"} {
			if strings.HasSuffix(word, suffix) && len(runes)-len([]rune(suffix)) >= 3 {
				words[i] = strings.TrimSuffix(word, suffix)
				break
			}
		}
		if words[i] == word && len(runes) >= 4 && strings.ContainsRune("аяеуюыийь", runes[len(runes)-1]) {
			words[i] = string(runes[:len(runes)-1])
		}
	}
	return words
}

func roomKey(text string) string { return strings.Join(roomWords(text), " ") }

func sameArea(a, b string) bool {
	return strings.EqualFold(strings.Join(strings.Fields(a), " "), strings.Join(strings.Fields(b), " "))
}

func ambiguousAreaNames(areas []string) bool {
	for i, a := range areas {
		for _, b := range areas[i+1:] {
			if roomKey(a) == roomKey(b) && !sameArea(a, b) {
				return true
			}
		}
	}
	return false
}

func wordSequence(words, needle []string) bool {
	if len(needle) == 0 || len(needle) > len(words) {
		return false
	}
	for i := 0; i+len(needle) <= len(words); i++ {
		if slices.Equal(words[i:i+len(needle)], needle) {
			return true
		}
	}
	return false
}

func mentionedAreas(text string, catalog []string, devices ...Entity) (areas []string, unknown, excluded bool) {
	words := roomWords(text)
	named := namedClimate(text, devices)
	// Match the longest room phrase at each position: "Летняя кухня" is not
	// a request for both that room and "Кухня". Disjoint mentions remain distinct.
	for i := 0; i < len(words); {
		longest := 0
		var matches []string
		for _, area := range catalog {
			name := roomWords(area)
			if len(name) == 0 || i+len(name) > len(words) || !slices.Equal(words[i:i+len(name)], name) {
				continue
			}
			if len(name) > longest {
				longest, matches = len(name), []string{area}
			} else if len(name) == longest {
				matches = append(matches, area)
			}
		}
		for _, area := range matches {
			if !slices.Contains(areas, area) {
				areas = append(areas, area)
			}
		}
		if longest > 0 {
			i += longest
		} else {
			i++
		}
	}
	// Exclusions need a richer parser; never turn them into a positive union.
	if slices.Contains(words, "кром") || slices.Contains(words, "исключа") || slices.Contains(words, "исключени") || slices.Contains(words, "исключ") ||
		slices.Contains(words, "не") || slices.Contains(words, "ни") || slices.Contains(words, "без") || slices.Contains(words, "вне") {
		return areas, false, true
	}
	for i, word := range words {
		if (word != "в" && word != "во" && word != "на") || i+1 == len(words) {
			continue
		}
		tail := words[i+1:]
		known := false
		for _, area := range catalog {
			name := roomWords(area)
			if len(tail) >= len(name) && slices.Equal(tail[:len(name)], name) {
				known = true
				break
			}
		}
		if known {
			continue
		}
		// A known climate-device name is not a room: "температура в колонке".
		// Keep checking later prepositions, so "в колонке в сарае" still refuses
		// the unknown explicit room instead of silently using the default.
		deviceReference := false
		for _, device := range named {
			for _, name := range append([]string{device.Name}, device.Aliases...) {
				key := roomWords(name)
				if len(key) > 0 && len(tail) >= len(key) && slices.Equal(tail[:len(key)], key) {
					deviceReference = true
				}
			}
		}
		if deviceReference {
			continue
		}
		// Settings and whole-home phrases are not unknown room names.
		if strings.IndexFunc(tail[0], unicode.IsDigit) >= 0 {
			// A number is not inherently a setting: "на 2 этаже" is a location.
			unit := ""
			if len(tail) > 1 {
				unit = tail[1]
			}
			if strings.HasSuffix(tail[0], "%") || slices.Contains([]string{"%", "процент", "градус", "кельвин", "ватт"}, unit) {
				continue
			}
			// "Сделай колонку на 37" names a known climate device and ends
			// in a number. A location such as "на 2 этаже" remains unknown.
			if word == "на" && len(named) > 0 && bareSettingTail(tail) {
				continue
			}
			unknown = true
			continue
		}
		if slices.Contains([]string{"всем", "всей", "всех", "весь", "все", "дом", "квартир", "максимум", "минимум", "половин", "процент"}, tail[0]) {
			continue
		}
		unknown = true
	}
	return areas, unknown, false
}

func bareSettingTail(words []string) bool {
	numbers := 0
	for _, word := range words {
		if word == roomKey("пожалуйста") {
			continue
		}
		if word == "" || strings.IndexFunc(word, func(r rune) bool { return !unicode.IsDigit(r) }) >= 0 {
			return false
		}
		numbers++
	}
	return numbers > 0
}

// scopedGroupAnswer validates the physical group membership against the entire
// selected inventory. A group service affects its real members, so dropping a
// member from metadata would not constrain the action. Unknown, out-of-room or
// cyclic membership must refuse the whole plan before the first service call.
func scopedGroupAnswer(plan Plan, known map[string]Entity) string {
	visited := map[string]uint8{}
	var closed func(string) bool
	closed = func(id string) bool {
		entity, exists := known[id]
		if !exists || visited[id] == 1 {
			return false
		}
		if visited[id] == 2 {
			return true
		}
		visited[id] = 1
		for _, member := range entity.Members {
			if !closed(member) {
				return false
			}
		}
		visited[id] = 2
		return true
	}
	for _, action := range plan.Actions {
		if len(known[action.EntityID].Members) > 0 && !closed(action.EntityID) {
			return "Состав этой группы выходит за пределы выбранной комнаты или известен не полностью. Какое отдельное устройство выбрать?"
		}
	}
	return ""
}

func wholeHome(text string) bool {
	s := roomKey(text)
	for _, phrase := range []string{"во всем доме", "во всей квартире", "по всему дому", "во всех комнатах", "везде"} {
		if wordSequence(strings.Fields(s), roomWords(phrase)) {
			return true
		}
	}
	return false
}

func entityInAreas(e Entity, selected, catalog []string) bool {
	if strings.TrimSpace(e.Area) != "" {
		for _, area := range selected {
			if sameArea(e.Area, area) {
				return true
			}
		}
		return false
	}
	var inferred []string
	for _, area := range catalog {
		for _, name := range append([]string{e.Name}, e.Aliases...) {
			if wordSequence(roomWords(name), roomWords(area)) {
				inferred = append(inferred, area)
				break
			}
		}
	}
	if len(inferred) != 1 {
		return false
	}
	for _, area := range selected {
		if sameArea(inferred[0], area) {
			return true
		}
	}
	return false
}

func namedClimate(text string, entities []Entity) []Entity {
	words := strings.Fields(normName(text))
	var named []Entity
	for _, e := range entities {
		if !strings.HasPrefix(e.ID, "climate.") {
			continue
		}
		for _, name := range append([]string{e.Name}, e.Aliases...) {
			key := normName(name)
			if key == "" || key == "температур" || key == "климат" {
				continue
			}
			if wordSequence(words, strings.Fields(key)) {
				named = append(named, e)
				break
			}
		}
	}
	return named
}

// scopedPlan only resolves simple singular on/off commands. Complex controls,
// groups, whole-home requests and parameters remain with the scoped planner.
func (c *Controller) scopedPlan(text, request string, s areaSelection) (Plan, bool) {
	if s.answer != "" {
		return Plan{Answer: s.answer}, true
	}
	if len(s.areas) != 1 {
		return Plan{}, false
	}
	roomNames := append([]string{}, c.names.areas()...)
	roomNames = append(roomNames, s.catalog...)
	roomNames = append(roomNames, s.areas...)
	if c.DefaultArea != "" {
		roomNames = append(roomNames, c.DefaultArea)
	}
	strip := func(text string) string {
		words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
		var out []string
		for i := 0; i < len(words); {
			start := i
			if slices.Contains([]string{"в", "во", "на"}, words[start]) && start+1 < len(words) {
				start++
			}
			matched := 0
			for _, area := range roomNames {
				name := roomWords(area)
				if len(name) > 0 && start+len(name) <= len(words) && slices.Equal(roomWords(strings.Join(words[start:start+len(name)], " ")), name) && len(name) > matched {
					matched = len(name)
				}
			}
			if matched > 0 {
				i = start + matched
				continue
			}
			if words[i] != "только" {
				out = append(out, words[i])
			}
			i++
		}
		return strings.Join(out, " ")
	}
	current := strip(text)
	clean := func(text string) string {
		words := strings.Fields(text)
		for len(words) > 0 && slices.Contains([]string{"алиса", "пожалуйста"}, words[0]) {
			words = words[1:]
		}
		if len(words) > 0 && words[len(words)-1] == "пожалуйста" {
			words = words[:len(words)-1]
		}
		return strings.Join(words, " ")
	}
	current = clean(current)
	// Check grammatical plurals before normName folds their last vowel.
	for _, word := range strings.Fields(current + " " + strip(request)) {
		if slices.Contains([]string{"светильники", "лампы", "подсветки", "люстры", "ленты"}, word) {
			return Plan{}, false
		}
	}
	service, target, ok := directCommand(current)
	if !ok && request != "" {
		previousService, previousTarget, previousOK := directCommand(clean(strip(request)))
		if !previousOK {
			return Plan{}, false
		}
		service, target, ok = previousService, normName(current), true
		if target == "" {
			target = previousTarget
		}
	}
	if !ok || target == "" || strings.IndexFunc(target, unicode.IsDigit) >= 0 {
		return Plan{}, false
	}
	for _, word := range strings.Fields(target) {
		if slices.Contains([]string{"свет", "освещени", "светильники", "лампы", "подсветки", "люстры", "ленты", "и", "или", "оба", "обе", "все", "всю", "весь", "всех", "сдела", "постав", "установ", "яркост", "процент", "ярче", "тусклее", "максимум", "минимум", "наполовин"}, word) {
			return Plan{}, false
		}
	}
	var exact, partial []Entity
	for _, e := range s.entities {
		domain, _, _ := strings.Cut(e.ID, ".")
		if !slices.Contains(directDomains, domain) {
			continue
		}
		isExact, isPartial := false, false
		for _, name := range append([]string{e.Name}, e.Aliases...) {
			key := normName(strip(name))
			isExact = isExact || key == target
			isPartial = isPartial || wordSequence(strings.Fields(key), strings.Fields(target))
		}
		if isExact {
			exact = append(exact, e)
		} else if isPartial {
			partial = append(partial, e)
		}
	}
	matches := exact
	if len(matches) == 0 {
		matches = partial
	}
	if len(matches) == 0 {
		return Plan{Answer: "В этой комнате не нашлось устройства с таким названием. Какое устройство вы имели в виду?"}, true
	}
	if len(matches) != 1 {
		return Plan{Answer: "В этой комнате подходят несколько устройств. Какое именно включить или выключить?"}, true
	}
	e := matches[0]
	if e.State == "unavailable" || e.State == "unknown" {
		return Plan{Answer: "Это устройство сейчас недоступно."}, true
	}
	domain, _, _ := strings.Cut(e.ID, ".")
	return Plan{Actions: []Action{{Domain: domain, Service: service, EntityID: e.ID, Data: map[string]any{}}}}, true
}
