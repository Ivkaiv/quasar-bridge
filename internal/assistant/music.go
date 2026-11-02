package assistant

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/url"
	"strings"

	"quasarbridge/internal/llm"
	"quasarbridge/internal/sound"
	"quasarbridge/internal/webaddress"
)

func musicDefinition() llm.Tool {
	return definition("play_music", "Включить музыку на текущей колонке с публичного сайта или по прямой ссылке на аудио. source — точный URL страницы, сайта или аудиофайла из запроса пользователя. Сам находит аудиоссылки в HTML и данных плеера, при необходимости открывает музыкальный раздел сайта и ставит найденные MP3 в очередь. Сайт «Зайцев нет» — https://zaycev.net/; для исполнителя или песни с этого сайта передай https://zaycev.net/search?query_search=<исполнитель и/или название в именительном падеже, URL-кодирование>. Не придумывай адреса файлов.", map[string]any{"source": map[string]string{"type": "string"}}, []string{"source"})
}

func (t *Tools) playMusic(ctx context.Context, args string) (any, error) {
	var p struct {
		Source string `json:"source"`
	}
	if json.Unmarshal([]byte(args), &p, json.RejectUnknownMembers(true)) != nil || p.Source == "" || len(p.Source) > 2048 {
		return nil, errors.New("invalid play_music source: provide the page or audio URL")
	}
	raw := webaddress.Normalize(strings.TrimSpace(p.Source))
	u, err := url.Parse(raw)
	if err != nil || validateWebURL(u) != nil || u.Fragment != "" {
		return nil, errors.New("invalid play_music source: public HTTP/HTTPS URL required")
	}
	if !sound.HasPlayer(ctx) {
		return nil, errors.New("play_music requires a current speaker request")
	}
	tracks := []WebLink{{URL: u.String()}}
	if zaycevSite(u) && mediaExtension(raw) == "" && zaycevPlayHash(raw) == "" {
		if tracks, err = t.WebReader.zaycevTracks(ctx, u.String()); err != nil {
			return nil, err
		}
	} else if mediaExtension(raw) == "" && zaycevPlayHash(raw) == "" {
		page, err := t.WebReader.Read(ctx, raw)
		if err != nil {
			return nil, err
		}
		tracks = page.Audio
		if len(tracks) == 0 {
			base, _ := url.Parse(page.URL)
			attempts := 0
			for _, link := range page.Links {
				next, err := url.Parse(link.URL)
				if err != nil || !strings.EqualFold(next.Host, base.Host) || next.Scheme != base.Scheme || next.String() == base.String() || !musicNavigation(link) {
					continue
				}
				attempts++
				section, err := t.WebReader.Read(ctx, link.URL)
				if err == nil && len(section.Audio) > 0 {
					tracks = section.Audio
					break
				}
				if attempts >= 3 {
					break
				}
			}
		}
	}
	if len(tracks) == 0 {
		return nil, errors.New("no playable audio links found on the page; a JavaScript-only player may require a direct audio URL")
	}
	if err := t.Playback.Start(ctx, tracks); err != nil {
		return nil, err
	}
	return map[string]any{"status": "queued", "track_count": len(tracks), "first_track": tracks[0].Title}, nil
}

func musicNavigation(link WebLink) bool {
	text := strings.ToLower(link.Title + " " + link.URL)
	for _, marker := range []string{"music", "audio", "songs", "playlist", "музык", "песн", "аудио"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}
