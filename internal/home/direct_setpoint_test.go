package home

import (
	"encoding/json/v2"
	"fmt"
	"testing"
	"unicode/utf8"
)

func testStationEntities() []Entity {
	return []Entity{
		{ID: "climate.opentherm_dhw", Name: "Горячая вода", Area: "Кухня", State: "heat", Aliases: []string{"Колонка", "Газовая колонка", "Водонагреватель"}},
		{ID: "climate.opentherm_heating", Name: "Отопление", Area: "Кухня", State: "heat", Aliases: []string{"Котёл", "Котел", "Батареи"}},
		{ID: "climate.kitchen_ac_kitchen_ac", Name: "Кондиционер в кухне", Area: "Кухня", State: "off"},
		{ID: "climate.kino_ac_kino_ac", Name: "Кондиционер в зале", Area: "Зал", State: "off"},
		{ID: "light.sonoff_led_sonoff_basic_relay", Name: "Подсветка над столом", Area: "Кухня", State: "on"},
		{ID: "light.sonoff_switch", Name: "Трек", Area: "Кухня", State: "off"},
		{ID: "light.led", Name: "Лента на кухне", Area: "Кухня", State: "off"},
		{ID: "light.tasmota_2", Name: "Лента в винокурне", Area: "Винокурня", State: "off"},
		{ID: "light.detskaia_nochnik", Name: "Ночник", Area: "Детская", State: "off"},
		{ID: "light.kabinet_embi_lampa", Name: "Эмби лампа", Area: "Кабинет", State: "off"},
		{ID: "light.svetilnik", Name: "Светильник", Area: "Холл", State: "off"},
		{ID: "media_player.hisense_50a5fe_eda26080_airplay", Name: "Телевизор", Area: "Гостиная", State: "off"},
		{ID: "media_player.mitv_afkr0", Name: "Приставка Xiaomi", Area: "Гостиная", State: "off"},
		{ID: "switch.breaker_relay", Name: "Рубильник", Area: "", State: "on"},
	}
}

func TestDirectSetpointKolonkaAndBoiler(t *testing.T) {
	entities := testStationEntities()
	c := &Controller{Direct: true, DefaultArea: "Кухня"}
	c.setNameIndex(entities)

	cases := []struct {
		text   string
		wantID string
		wantT  float64
	}{
		{"сделай колонку 41", "climate.opentherm_dhw", 41},
		{"поставь колонку на 42 градуса", "climate.opentherm_dhw", 42},
		{"Алиса, сделай температуру в колонке 39", "climate.opentherm_dhw", 39},
		{"установи горячую воду 40", "climate.opentherm_dhw", 40},
		{"сделай котёл 22", "climate.opentherm_heating", 22},
		{"поставь отопление на 21,5", "climate.opentherm_heating", 21.5},
	}
	for _, tc := range cases {
		plan, e, ok := c.directSetpointPlan(tc.text)
		if !ok {
			t.Fatalf("%q: expected direct setpoint hit", tc.text)
		}
		if e.ID != tc.wantID || len(plan.Actions) != 1 || plan.Actions[0].Service != "set_temperature" || plan.Actions[0].Data["temperature"] != tc.wantT {
			t.Fatalf("%q: got entity=%s actions=%+v", tc.text, e.ID, plan.Actions)
		}
	}

	// Even if HA aliases were missing, "сделай колонку 41" still picks DHW unambiguously.
	noAliases := []Entity{
		{ID: "climate.opentherm_dhw", Name: "Горячая вода", Area: "Кухня", State: "heat"},
		{ID: "climate.opentherm_heating", Name: "Отопление", Area: "Кухня", State: "heat"},
		{ID: "climate.kitchen_ac_kitchen_ac", Name: "Кондиционер в кухне", Area: "Кухня", State: "off"},
	}
	if plan, e, ok := matchSetpointEntity("сделай колонку 41", noAliases); !ok || e.ID != "climate.opentherm_dhw" || plan.Actions[0].Data["temperature"] != float64(41) {
		t.Fatalf("fallback without aliases failed: ok=%v e=%+v plan=%+v", ok, e, plan)
	}

	// Relative adjustments must stay with the planner.
	if _, _, ok := c.directSetpointPlan("сделай колонку теплее на 2 градуса"); ok {
		t.Fatal("relative temperature change must not use direct setpoint")
	}
}

func TestHomeHintDistinguishesSongsFromDevices(t *testing.T) {
	c := &Controller{Direct: true, DefaultArea: "Кухня"}
	c.setNameIndex(testStationEntities())

	songs := []string{
		"включи Батарейку",
		"включи Фиксики Часики",
		"включи Светлана Лобода",
		"включи Солнце Монако",
		"включи Звезда по имени Солнце",
		"поставь Король и Шут Кукла колдуна",
		"включи Белая ночь",
		"включи Группа крови",
		"включи Ветер перемен",
	}
	for _, s := range songs {
		known, matches, summary := c.HomeHint(s)
		if !known || summary == "" {
			t.Fatalf("%q: expected populated index", s)
		}
		if matches {
			t.Errorf("%q: song falsely matched smart-home device/room", s)
		}
	}

	devices := []string{
		"включи свет",
		"включи подсветку",
		"включи ночник",
		"включи приставку",
		"включи телевизор",
		"включи эмби лампу",
		"включи ленту в винокурне",
		"включи рубильник",
		"включи кондиционер в зале",
		"сделай колонку 41",
		"включи батареи",
	}
	for _, d := range devices {
		known, matches, _ := c.HomeHint(d)
		if !known || !matches {
			t.Errorf("%q: real smart-home command missed by HomeHint", d)
		}
	}
}

func TestBuildCatalogSummaryValidUTF8AndJSON(t *testing.T) {
	var many []Entity
	for i := 0; i < 80; i++ {
		many = append(many, Entity{
			ID:      fmt.Sprintf("light.dev_%d", i),
			Name:    fmt.Sprintf("Длинное русское название светильника номер %d", i),
			Area:    fmt.Sprintf("Комната %d", i%12),
			Aliases: []string{"Подсветка", "Основной свет"},
		})
	}
	c := &Controller{Direct: true, DefaultArea: "Кухня"}
	c.setNameIndex(many)
	known, _, summary := c.HomeHint("включи свет")
	if !known || summary == "" {
		t.Fatal("expected non-empty summary")
	}
	if !utf8.ValidString(summary) {
		t.Fatalf("summary contains invalid UTF-8: %q", summary)
	}
	if len([]rune(summary)) > 900 {
		t.Fatalf("summary exceeded rune cap: %d", len([]rune(summary)))
	}
	if _, err := json.Marshal(map[string]string{"content": summary}); err != nil {
		t.Fatalf("encoding/json/v2 failed to marshal summary: %v", err)
	}
}
