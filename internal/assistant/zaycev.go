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
	"time"
	"unicode"

	"golang.org/x/net/html"
)

// Queues retain the site's stable playback handle, never the signed MP3 URL.
// The handle is exchanged for a fresh address just before each track streams.
const zaycevAPI = "https://zaycev.net/api/external/"
const zaycevPlayPrefix = "/api/external/track/play/"

var zaycevHash = regexp.MustCompile(`^[0-9a-f]{8,64}$`)

func zaycevSite(u *url.URL) bool {
	if u == nil || validateWebURL(u) != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	return (host == "zaycev.net" || host == "www.zaycev.net") &&
		(u.Port() == "" || u.Scheme == "https" && u.Port() == "443" || u.Scheme == "http" && u.Port() == "80")
}

func zaycevPlayHash(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || !zaycevSite(u) || u.Scheme != "https" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || !strings.HasPrefix(u.Path, zaycevPlayPrefix) {
		return ""
	}
	hash := strings.TrimPrefix(u.Path, zaycevPlayPrefix)
	if !zaycevHash.MatchString(hash) {
		return ""
	}
	return hash
}

func queueableTrack(raw string) bool {
	return mediaExtension(raw) == ".mp3" || zaycevPlayHash(raw) != ""
}

func zaycevSearchQuery(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || !zaycevSite(u) || strings.TrimSuffix(u.Path, "/") != "/search" {
		return ""
	}
	return strings.TrimSpace(cmp.Or(u.Query().Get("query_search"), u.Query().Get("q")))
}

// Only a command naming the site alone takes the direct start path. Commands
// naming an artist or song stay with the assistant to construct a search URL.
func zaycevSpokenPlain(text string) bool {
	named := false
	for _, word := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && r != '-' }) {
		switch word {
		case "зайцев", "зайцева", "зайцеве", "zaycev":
			named = true
		case "алиса", "пожалуйста", "включи", "включите", "проиграй", "проиграйте", "воспроизведи", "воспроизведите", "поставь", "поставьте", "запусти", "запустите", "сыграй", "сыграйте", "мне", "нам", "с", "со", "на", "из", "сайт", "сайта", "сайте", "нет", "net", "точка", "музыку", "музыка", "песни", "песню", "трек", "треки", "аудио", "какую-нибудь", "любую", "популярную", "популярные":
		default:
			return false
		}
	}
	return named
}

func zaycevWords(text string) []string {
	var words []string
	for _, word := range strings.FieldsFunc(strings.ReplaceAll(strings.ToLower(text), "ё", "е"), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		runes := []rune(word)
		if len(runes) >= 2 {
			words = append(words, string(runes[:min(len(runes), 4)]))
		}
	}
	return words
}

// Stems tolerate common Russian inflections (for example, Басты / Баста).
func zaycevRelevant(query, title string) bool {
	have := zaycevWords(title)
	want := zaycevWords(query)
	for _, word := range want {
		if !slices.Contains(have, word) {
			return false
		}
	}
	return len(want) > 0
}

// Keep all metadata calls, including POST, behind the reader's transport and
// redirect policy. Metadata redirects additionally stay on the HTTPS site;
// signed audio is fetched separately through the queue's existing SSRF guard.
func (w *WebReader) zaycevRequest(ctx context.Context, method, raw string, body any, limit int64) ([]byte, error) {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 4096 || !zaycevSite(u) || u.Fragment != "" || body != nil && u.Scheme != "https" {
		return nil, errors.New("zaycev.net metadata address is blocked")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(w.config.WebTimeoutMS)*time.Millisecond)
	defer cancel()
	var payload io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, errors.New("invalid zaycev.net metadata request")
		}
		payload = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), payload)
	if err != nil {
		return nil, errors.New("invalid zaycev.net metadata request")
	}
	req.Header.Set("User-Agent", "QuasarBridge/0.7 (music reader)")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := *w.http
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 4 || !zaycevSite(req.URL) || req.URL.Scheme != "https" {
			return errors.New("zaycev.net metadata redirect is blocked")
		}
		if w.http.CheckRedirect != nil {
			return w.http.CheckRedirect(req, via)
		}
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("zaycev.net request failed or target blocked")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("zaycev.net returned an unsuccessful HTTP status")
	}
	limit = min(limit, int64(w.config.WebMaxBytes))
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("zaycev.net response is unreadable or too large")
	}
	return data, nil
}

type zaycevTrackInfo struct {
	Track           string `json:"track"`
	ArtistName      string `json:"artistName"`
	PlaybackEnabled bool   `json:"playbackEnabled"`
}

func zaycevPageTracks(page []byte) ([]int64, map[string]zaycevTrackInfo, error) {
	z := html.NewTokenizer(bytes.NewReader(page))
	for kind := z.Next(); kind != html.ErrorToken; kind = z.Next() {
		token := z.Token()
		if kind != html.StartTagToken || token.Data != "script" {
			continue
		}
		for _, attr := range token.Attr {
			if attr.Key != "id" || attr.Val != "__NEXT_DATA__" || z.Next() != html.TextToken {
				continue
			}
			var data struct {
				Props struct {
					State struct {
						Playlist struct {
							Types    []string                   `json:"types"`
							Info     map[string]zaycevTrackInfo `json:"info"`
							TrackIDs map[string][]int64         `json:"trackIds"`
						} `json:"playlist"`
					} `json:"initialReduxState"`
				} `json:"props"`
			}
			if json.Unmarshal(z.Text(), &data) != nil {
				return nil, nil, errors.New("zaycev.net page track list is unreadable")
			}
			playlist := data.Props.State.Playlist
			var ids []int64
			for _, kind := range playlist.Types {
				for _, id := range playlist.TrackIDs[kind] {
					if !slices.Contains(ids, id) {
						ids = append(ids, id)
					}
					if len(ids) >= 64 {
						return ids, playlist.Info, nil
					}
				}
			}
			return ids, playlist.Info, nil
		}
	}
	return nil, nil, errors.New("zaycev.net page has no track list")
}

func (w *WebReader) zaycevTracks(ctx context.Context, raw string) ([]WebLink, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(w.config.WebTimeoutMS)*time.Millisecond)
	defer cancel()
	var ids []int64
	var info map[string]zaycevTrackInfo
	query := zaycevSearchQuery(raw)
	if query != "" {
		body, err := w.zaycevRequest(ctx, http.MethodGet, zaycevAPI+"pages/search/tracks?"+url.Values{"q": {query}, "page": {"1"}, "limit": {"30"}}.Encode(), nil, int64(w.config.WebMaxBytes))
		if err != nil {
			return nil, err
		}
		var found struct {
			TrackIDs []int64                    `json:"trackIds"`
			Info     map[string]zaycevTrackInfo `json:"tracksInfo"`
			Popular  bool                       `json:"popularInSearch"`
		}
		if json.Unmarshal(body, &found) != nil {
			return nil, errors.New("zaycev.net search response is unreadable")
		}
		if !found.Popular {
			ids, info = found.TrackIDs, found.Info
		}
	} else {
		page, err := w.zaycevRequest(ctx, http.MethodGet, raw, nil, int64(w.config.WebMaxBytes))
		if err != nil {
			return nil, err
		}
		ids, info, err = zaycevPageTracks(page)
		if err != nil {
			return nil, err
		}
	}
	titles := make(map[int64]string)
	var selected []int64
	for _, id := range ids {
		track := info[strconv.FormatInt(id, 10)]
		title := strings.TrimSpace(track.ArtistName + " — " + track.Track)
		if id <= 0 || !track.PlaybackEnabled || strings.TrimSpace(track.Track) == "" || titles[id] != "" || query != "" && !zaycevRelevant(query, title) {
			continue
		}
		if runes := []rune(title); len(runes) > 300 {
			title = string(runes[:300])
		}
		titles[id] = title
		selected = append(selected, id)
		if len(selected) == 64 {
			break
		}
	}
	if len(selected) == 0 {
		return nil, errors.New("no playable tracks found on zaycev.net for this page or search")
	}
	body, err := w.zaycevRequest(ctx, http.MethodPost, zaycevAPI+"track/filezmeta", map[string]any{"trackIds": selected, "subscription": false}, 1<<20)
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
	hashes := make(map[int64]string)
	for _, track := range meta.Tracks {
		if zaycevHash.MatchString(track.Streaming) {
			hashes[track.ID] = track.Streaming
		}
	}
	var tracks []WebLink
	for _, id := range selected {
		if hash := hashes[id]; hash != "" {
			tracks = append(tracks, WebLink{URL: "https://zaycev.net" + zaycevPlayPrefix + hash, Title: titles[id]})
		}
	}
	if len(tracks) == 0 {
		return nil, errors.New("zaycev.net returned no streamable tracks")
	}
	return tracks, nil
}

func (w *WebReader) resolveZaycev(ctx context.Context, hash string) (string, error) {
	if !zaycevHash.MatchString(hash) {
		return "", errors.New("invalid zaycev.net playback handle")
	}
	body, err := w.zaycevRequest(ctx, http.MethodGet, zaycevAPI+"track/play/"+hash, nil, 64<<10)
	if err != nil {
		return "", err
	}
	var play struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(body, &play) != nil || len(play.URL) > 4096 {
		return "", errors.New("zaycev.net returned no valid track address")
	}
	u, err := url.Parse(play.URL)
	if err != nil || validateWebURL(u) != nil || u.Fragment != "" {
		return "", errors.New("zaycev.net track address is blocked")
	}
	return u.String(), nil
}
