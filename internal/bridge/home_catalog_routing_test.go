package bridge

import (
	"context"
	"strings"
	"testing"

	"quasarbridge/internal/llm"
)

type stubCatalogHome struct {
	matchFn func(string) bool
	summary string
}

func (s stubCatalogHome) Handle(context.Context, string) (string, error) { return "Готово.", nil }
func (s stubCatalogHome) HomeHint(text string) (bool, bool, string) {
	return true, s.matchFn(text), s.summary
}

func TestBareSongTitlesSkipClassifierAndRouteToYandex(t *testing.T) {
	llmCalls := 0
	e := &Engine{
		Home: stubCatalogHome{
			matchFn: func(text string) bool {
				s := strings.ToLower(text)
				return strings.Contains(s, "подсветк") || strings.Contains(s, "приставк")
			},
			summary: "Комнаты: Кухня, Гостиная. Устройства: Подсветка над столом, Приставка Xiaomi, Горячая вода (Колонка).",
		},
		LLM: completionFunc(func(ctx context.Context, m []llm.Message, structured bool) (string, error) {
			llmCalls++
			return `{"route":"home"}`, nil
		}),
	}
	for _, song := range []string{"включи Батарейку", "включи Фиксики Часики", "поставь Солнце Монако", "Алиса, включи Группа крови"} {
		d := e.Decide(t.Context(), song)
		if d.Route != "yandex" || d.Source != "non_home_media" {
			t.Fatalf("%q: got route=%s source=%s, want yandex/non_home_media", song, d.Route, d.Source)
		}
	}
	if llmCalls != 0 {
		t.Fatalf("expected 0 classifier calls for non-home bare media, got %d", llmCalls)
	}

	// Real device without a static rule keyword reaches classifier with the HA device catalog in prompt.
	var seenPrompt string
	e.LLM = completionFunc(func(ctx context.Context, m []llm.Message, structured bool) (string, error) {
		seenPrompt = m[0].Content
		return `{"route":"home"}`, nil
	})
	d := e.Decide(t.Context(), "включи приставку")
	if d.Route != "home" || d.Source != "classifier" {
		t.Fatalf("device routing broken: %+v", d)
	}
	if !strings.Contains(seenPrompt, "Приставка Xiaomi") {
		t.Fatalf("classifier prompt missing HA device catalog: %s", seenPrompt)
	}
}
