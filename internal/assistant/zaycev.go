package assistant

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// zaycev.net pages list tracks only by id; the MP3 address comes from the
// site API and carries a short-lived token, so it is resolved right before
// each track is streamed.
const zaycevAPI = "https://zaycev.net/api/external/"
const zaycevPlayPrefix = "/api/external/track/play/"

var (
	zaycevNextData = regexp.MustCompile(`(?s)<script id="__NEXT_DATA__"[^>]*>(.*?)</script>`)
	zaycevTrack    = regexp.MustCompile(`"(\d+)":\{"size":[^{}]*?"track":"((?:[^"\\]|\\.)*)"[^{}]*?"artistName":"((?:[^"\\]|\\.)*)"[^{}]*?"playbackEnabled":true`)
	zaycevHash     = regexp.MustCompile(`^[0-9a-f]{8,64}$`)
)

// zaycevSpoken recognizes the site named aloud ("с сайта Зайцев нет"),
// which speech recognition does not turn into a domain.
func zaycevSpoken(text string) bool {
	s := strings.ToLower(text)
	return strings.Contains(s, "зайцев") || strings.Contains(s, "zaycev")
}

// zaycevSearchQuery returns the search text of a zaycev.net search URL.
func zaycevSearchQuery(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || !zaycevSite(u) || strings.TrimSuffix(u.Path, "/") != "/search" {
		return ""
	}
	q := u.Query()
	return strings.TrimSpace(cmp.Or(q.Get("query_search"), q.Get("q")))
}

// zaycevFiller lists words of a spoken command that do not name the music.
var zaycevFiller = map[string]bool{}

func init() {
	for _, w := range strings.Fields("алиса пожалуйста включи включите проиграй воспроизведи поставь запусти сыграй мне нам с со на из сайт сайта сайте зайцев зайцева зайцеве нет net zaycev точка ру музыку музыка песни песню трек треки аудио какую-нибудь любую популярную популярные") {
		zaycevFiller[w] = true
	}
}

// zaycevSpokenPlain reports a spoken command that names only the site, so the
// main page can be started without asking the model what to search for.
func zaycevSpokenPlain(text string) bool {
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && r != '-' }) {
		if !zaycevFiller[w] {
			return false
		}
	}
	return true
}

func zaycevWords(text string) []string {
	var words []string
	for _, w := range strings.FieldsFunc(strings.ToLower(strings.ReplaceAll(text, "ё", "е")), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if r := []rune(w); len(r) >= 3 {
			words = append(words, string(r[:min(len(r), 4)]))
		}
	}
	return words
}

// zaycevRelevant compares word stems, so "Басты" still matches "Баста".
func zaycevRelevant(query, title string) bool {
	have := zaycevWords(title)
	for _, w := range zaycevWords(query) {
		if slices.Contains(have, w) {
			return true
		}
	}
	return false
}

func zaycevSite(u *url.URL) bool {
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	return host == "zaycev.net" || host == "www.zaycev.net"
}

// zaycevPlayHash returns the streaming hash of a queued zaycev.net track URL.
func zaycevPlayHash(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || !zaycevSite(u) || u.RawQuery != "" || !strings.HasPrefix(u.Path, zaycevPlayPrefix) {
		return ""
	}
	hash := strings.TrimPrefix(u.Path, zaycevPlayPrefix)
	if !zaycevHash.MatchString(hash) {
		return ""
	}
	return hash
}

// queueableTrack reports whether a track can be streamed through MusicQueue.
func queueableTrack(raw string) bool {
	return mediaExtension(raw) == ".mp3" || zaycevPlayHash(raw) != ""
}

func zaycevRequest(ctx context.Context, client *http.Client, method, raw string, body any, limit int64) ([]byte, error) {
	var payload io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		payload = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, raw, payload)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (QuasarBridge music)")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("zaycev.net request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("zaycev.net returned an unsuccessful HTTP status")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("zaycev.net response is unreadable or too large")
	}
	return data, nil
}

// zaycevTracks lists playable tracks of a zaycev.net page as queue URLs.
func (w *WebReader) zaycevTracks(ctx context.Context, raw string) ([]WebLink, error) {
	var list []byte
	var order []int64
	query := zaycevSearchQuery(raw)
	if query != "" {
		// The search page is rendered in the browser; its API returns the same
		// track descriptions plus the relevance order.
		body, err := zaycevRequest(ctx, w.http, http.MethodGet, zaycevAPI+"pages/search/tracks?"+url.Values{"q": {query}, "page": {"1"}, "limit": {"30"}}.Encode(), nil, int64(w.config.WebMaxBytes))
		if err != nil {
			return nil, err
		}
		var found struct {
			TrackIDs []int64 `json:"trackIds"`
		}
		_ = json.Unmarshal(body, &found)
		list, order = body, found.TrackIDs
	} else {
		page, err := zaycevRequest(ctx, w.http, http.MethodGet, raw, nil, int64(w.config.WebMaxBytes))
		if err != nil {
			return nil, err
		}
		data := zaycevNextData.FindSubmatch(page)
		if data == nil {
			return nil, errors.New("zaycev.net page has no track list")
		}
		list = data[1]
	}
	titles := map[int64]string{}
	var ids []int64
	for _, m := range zaycevTrack.FindAllSubmatch(list, -1) {
		id, err := strconv.ParseInt(string(m[1]), 10, 64)
		if err != nil || titles[id] != "" {
			continue
		}
		track, _ := strconv.Unquote(`"` + string(m[2]) + `"`)
		artist, _ := strconv.Unquote(`"` + string(m[3]) + `"`)
		titles[id] = strings.TrimSpace(artist + " — " + track)
		ids = append(ids, id)
		if len(ids) == 64 {
			break
		}
	}
	if len(order) > 0 {
		var ranked []int64
		for _, id := range order {
			if titles[id] != "" && !slices.Contains(ranked, id) {
				ranked = append(ranked, id)
			}
		}
		ids = ranked
	}
	if query != "" {
		// Without a match the site answers with unrelated popular tracks;
		// keep only results that share a word with the request.
		var relevant []int64
		for _, id := range ids {
			if zaycevRelevant(query, titles[id]) {
				relevant = append(relevant, id)
			}
		}
		ids = relevant
	}
	if len(ids) == 0 {
		return nil, errors.New("no playable tracks found on zaycev.net for this page or search")
	}
	body, err := zaycevRequest(ctx, w.http, http.MethodPost, zaycevAPI+"track/filezmeta", map[string]any{"trackIds": ids, "subscription": false}, 1<<20)
	if err != nil {
		return nil, err
	}
	var meta struct {
		Tracks []struct {
			ID        int64  `json:"id"`
			Streaming string `json:"streaming"`
		} `json:"tracks"`
	}
	if json.Unmarshal(body, &meta) != nil {
		return nil, errors.New("zaycev.net track metadata is unreadable")
	}
	hashes := map[int64]string{}
	for _, t := range meta.Tracks {
		if zaycevHash.MatchString(t.Streaming) {
			hashes[t.ID] = t.Streaming
		}
	}
	var tracks []WebLink
	for _, id := range ids {
		if hash := hashes[id]; hash != "" {
			tracks = append(tracks, WebLink{URL: "https://zaycev.net" + zaycevPlayPrefix + hash, Title: titles[id]})
		}
	}
	if len(tracks) == 0 {
		return nil, errors.New("zaycev.net returned no streamable tracks")
	}
	return tracks, nil
}

// resolveZaycev exchanges a queued track URL for its current MP3 address.
func resolveZaycev(ctx context.Context, client *http.Client, hash string) (string, error) {
	body, err := zaycevRequest(ctx, client, http.MethodGet, zaycevAPI+"track/play/"+hash, nil, 64<<10)
	if err != nil {
		return "", err
	}
	var play struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(body, &play) != nil || play.URL == "" {
		return "", errors.New("zaycev.net returned no track address")
	}
	u, err := url.Parse(play.URL)
	if err != nil || validateWebURL(u) != nil {
		return "", errors.New("zaycev.net track address is blocked")
	}
	return u.String(), nil
}
