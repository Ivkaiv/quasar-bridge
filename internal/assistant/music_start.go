package assistant

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/url"
	"regexp"
	"strings"

	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/webaddress"
)

var musicStartCommand = regexp.MustCompile(`(?i)^(?:(?:алиса|пожалуйста|спроси нейросеть|спроси свою модель)[\s,;:]+)*(?:включи(?:те)?|проиграй(?:те)?|воспроизведи(?:те)?|поставь(?:те)?|запусти(?:те)?|сыграй(?:те)?)(?:\s|$)`)

// CanStartMusic recognizes a current explicit command without consulting history.
func (t *Tools) CanStartMusic(text string) bool {
	return t != nil && t.Config.PlaySound && t.Config.Web && t.Playback != nil && musicStartSource(text) != ""
}

func musicStartSource(text string) string {
	plainZaycev := zaycevSpokenPlain(text)
	// The site-only allowlist excludes negations and artist/song names.
	if (!webaddress.MusicRequest(text) && !plainZaycev) || !musicStartCommand.MatchString(dialogue.Normalize(text)) {
		return ""
	}
	source := strings.TrimRight(explicitURL.FindString(webaddress.Normalize(text)), ".,!?;:)]}")
	if source == "" && plainZaycev {
		source = "https://zaycev.net/"
	}
	u, err := url.Parse(source)
	if err != nil || len(source) > 2048 || validateWebURL(u) != nil || u.Fragment != "" {
		return ""
	}
	return source
}

func (t *Tools) StartMusic(ctx context.Context, text string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !t.CanStartMusic(text) {
		return "", errors.New("music playback requires an explicit current command and website URL")
	}
	args, err := json.Marshal(map[string]string{"source": musicStartSource(text)})
	if err != nil {
		return "", err
	}
	if _, err := t.Call(ctx, "play_music", string(args)); err != nil {
		return "", err
	}
	return "Включаю музыку.", nil
}
