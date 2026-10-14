package gmaps

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gosom/scrapemate"
)

/*
The place's whole photo album, which the page itself does not carry.

The embedded gallery in `Entry.Images` is four photographs and a cover, and that is all the page
state holds however it is read. The album behind them — 344 photographs for one restaurant in
Brasília, 3140 for another — lives behind the same internal endpoint the reviews use, and reaching
it costs nothing: it is a Maps page talking to Maps, not a billed API call.

It has to be fetched **inside the browser**. Measured before writing any of this, against three
known places: the scrapemate stealth client gets HTTP 200 and the true photo count, and an empty
photo array. Seven variations of the request — page size, filter flags, a different entity kind —
all answered the same way, so the truncation is the session, not the query. The browser's own
`fetch` with `credentials: 'include'` returns 147 photographs for the same place, which is the
same route `fetchWithBrowser` already takes for reviews.
*/

const (
	// How many photographs to keep per place. Ten is the storage budget; the surplus covers the
	// kinds that get filtered (panoramas, Street View tiles) and URLs that die before download.
	albumDefaultLimit = 24
	albumMaxLimit     = 60

	// Photo kinds, as the entity array reports them at index 2.
	albumKindUserPhoto = 12

	// Long enough for the page to settle, short enough that a batch of sixteen places does not
	// notice it.
	albumRetryDelay = 900 * time.Millisecond
)

// AlbumPhoto is one photograph from the album listing.
type AlbumPhoto struct {
	ID string `json:"id"`
	// Direct lh3 URL, still carrying whatever size directive the listing chose. Whoever stores it
	// rewrites that directive; see the Node side's placePhotoVariants.
	URL    string `json:"url"`
	Kind   int    `json:"kind"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// albumPhotoLimit reports how many album photographs to keep per place.
//
// Read from the environment rather than carried on the job, because threading a new job field
// would touch every runner's CreateSeedJobs signature for a setting that is the same on every job
// a deployment runs. Zero disables the fetch, which is the default: a plain scrape should not pay
// for a request it has no use for.
func albumPhotoLimit() int {
	raw := strings.TrimSpace(os.Getenv("GMAPS_ALBUM_PHOTO_LIMIT"))
	if raw == "" {
		return 0
	}

	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed < 0 {
		return 0
	}

	if parsed > albumMaxLimit {
		return albumMaxLimit
	}

	return parsed
}

// albumURL builds the listing request for a place's feature id — the `data_id` the scrape already
// extracts, e.g. `0x935a3002feee27b1:0x7bf9e264080130b4`.
func albumURL(ftid, lang, gl string) string {
	if lang == "" {
		lang = "en"
	}

	if gl == "" {
		gl = "us"
	}

	pb := fmt.Sprintf("!1e2!3m3!1s%s!2s%s!5s!6m1!1e1", url.QueryEscape(ftid), lang)

	return fmt.Sprintf(
		"https://www.google.com/maps/rpc/photo/listentityphotos?authuser=0&hl=%s&gl=%s&pb=%s",
		url.QueryEscape(lang), url.QueryEscape(gl), pb,
	)
}

// FetchAlbum returns up to limit photographs for a place, newest-session first.
//
// A place with no album is not an error: it returns no photographs and no error, so the caller
// can tell "this place has nothing" from "we could not ask". The small restaurant that has five
// photographs in total must come back with five, not with a failure.
func FetchAlbum(
	ctx context.Context,
	page scrapemate.BrowserPage,
	ftid, lang, gl string,
	limit int,
) ([]AlbumPhoto, error) {
	if limit <= 0 || ftid == "" {
		return nil, nil
	}

	if page == nil {
		return nil, fmt.Errorf("album listing needs a browser page: the stealth client is served an empty array")
	}

	u := albumURL(ftid, lang, gl)

	/*
	   Asked twice when the first answer contradicts itself.

	   The listing reports the place's true photo count even when it withholds the photographs, so
	   "no photographs, but 5 exist" is a refusal and "no photographs, and none exist" is a fact.
	   Observed on consecutive runs over the same four places: the same place answered with 4
	   photographs, then with 1, then with none, while its stated total never moved — the request
	   races the page's own session, and asking again on a page that is already open costs one
	   request and no page time.

	   One retry, not a loop: if the second answer is as thin as the first, the place keeps the
	   photographs the page itself gave us, which is what it would have had anyway.
	*/
	photos, total, err := fetchAndParseAlbum(ctx, page, u)
	if err != nil {
		return nil, err
	}

	selected := selectAlbumPhotos(photos, limit)

	if len(selected) == 0 && total > 0 {
		time.Sleep(albumRetryDelay)

		retried, retriedTotal, retryErr := fetchAndParseAlbum(ctx, page, u)
		if retryErr == nil {
			if again := selectAlbumPhotos(retried, limit); len(again) > 0 {
				return again, nil
			}

			total = retriedTotal
		}
	}

	if len(selected) == 0 {
		fmt.Printf("album: %s returned no usable photographs (listed=%d total=%d)\n", ftid, len(photos), total)
	}

	return selected, nil
}

// fetchAndParseAlbum is one round trip: ask the page, read the answer.
func fetchAndParseAlbum(
	ctx context.Context,
	page scrapemate.BrowserPage,
	u string,
) ([]AlbumPhoto, int, error) {
	body, err := fetchAlbumWithBrowser(ctx, page, u)
	if err != nil {
		return nil, 0, err
	}

	return parseAlbumResponse(body)
}

// fetchAlbumWithBrowser runs the request inside the page, which is what carries the cookies.
func fetchAlbumWithBrowser(_ context.Context, page scrapemate.BrowserPage, u string) ([]byte, error) {
	js := fmt.Sprintf(`async () => {
		try {
			const response = await fetch('%s', {
				method: 'GET',
				credentials: 'include',
				headers: { 'Accept': '*/*' }
			});
			if (!response.ok) {
				return { error: 'HTTP ' + response.status };
			}
			return { data: await response.text() };
		} catch (e) {
			return { error: e.message };
		}
	}`, u)

	result, err := page.Eval(js)
	if err != nil {
		return nil, fmt.Errorf("album fetch failed: %w", err)
	}

	resultMap, ok := result.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("album fetch returned %T", result)
	}

	if errMsg, hasError := resultMap["error"]; hasError {
		return nil, fmt.Errorf("album fetch error: %v", errMsg)
	}

	data, ok := resultMap["data"].(string)
	if !ok || len(data) < 10 {
		return nil, fmt.Errorf("album fetch returned an empty body")
	}

	return []byte(data), nil
}

/*
parseAlbumResponse reads the listing into photographs and the place's true total.

The answer is the usual anti-hijacking prefix followed by an array: index 0 holds the photographs
and index 1 the total the place has, which is reported even when the photographs are withheld.
That combination — a total with an empty array — is exactly what an unauthenticated request
returns, so it is handled as "nothing to show", not as a parse failure: it is the signal that the
session was wrong, and the caller decides what to do about it.
*/
//nolint:gomnd // it's ok, I need the indexes
func parseAlbumResponse(data []byte) (photos []AlbumPhoto, total int, err error) {
	trimmed := trimJSONPrefix(string(data))

	var parsed []any
	if err := json.Unmarshal(trimmed, &parsed); err != nil {
		return nil, 0, fmt.Errorf("album response is not json: %w", err)
	}

	if len(parsed) > 1 {
		if value, ok := parsed[1].(float64); ok {
			total = int(value)
		}
	}

	if len(parsed) == 0 {
		return nil, total, nil
	}

	items, ok := parsed[0].([]any)
	if !ok {
		return nil, total, nil
	}

	for i := range items {
		item := getNthElementAndCast[[]any](items, i)
		if len(item) == 0 {
			continue
		}

		link := getNthElementAndCast[string](item, 6, 0)
		if link == "" {
			continue
		}

		photo := AlbumPhoto{
			ID:   getNthElementAndCast[string](item, 0),
			URL:  link,
			Kind: int(getNthElementAndCast[float64](item, 2)),
		}

		dims := getNthElementAndCast[[]any](item, 6, 2)
		if len(dims) == 2 {
			photo.Width = int(getNthElementAndCast[float64](dims, 0))
			photo.Height = int(getNthElementAndCast[float64](dims, 1))
		}

		photos = append(photos, photo)
	}

	return photos, total, nil
}

/*
selectAlbumPhotos keeps the photographs worth storing, best first.

Three rules, in order of how much they matter:

Street View tiles go first. They are served from a host that answers 403 to anything but Maps
itself, so keeping one means a queued photo that can never download — 1239 of them in one past
sweep, which is what the unexplained "403 problem" in the run notes actually was.

User photographs outrank everything else, but only as a preference: a place whose whole album is
five panoramas must still come back with five. Filtering kind strictly is how a small place ends
up with nothing.

Then biggest first, by pixel area. Whoever stores these rewrites the size directive to ask for the
original, and that request never upscales — so native resolution is the only thing that decides
what the stored photograph will actually look like.
*/
func selectAlbumPhotos(photos []AlbumPhoto, limit int) []AlbumPhoto {
	if limit <= 0 {
		return nil
	}

	preferred := make([]AlbumPhoto, 0, len(photos))
	fallback := make([]AlbumPhoto, 0, len(photos))
	seen := make(map[string]struct{}, len(photos))

	for _, photo := range photos {
		if photo.URL == "" || isStreetViewPhotoURL(photo.URL) {
			continue
		}

		key := photo.ID
		if key == "" {
			key = photoURLIdentity(photo.URL)
		}

		if _, done := seen[key]; done {
			continue
		}

		seen[key] = struct{}{}

		if photo.Kind == albumKindUserPhoto {
			preferred = append(preferred, photo)
			continue
		}

		fallback = append(fallback, photo)
	}

	sortAlbumPhotosByArea(preferred)
	sortAlbumPhotosByArea(fallback)

	selected := preferred
	if len(selected) < limit {
		selected = append(selected, fallback...)
	}

	if len(selected) > limit {
		selected = selected[:limit]
	}

	return selected
}

// sortAlbumPhotosByArea orders by pixel area, descending, keeping the album's own order on ties.
func sortAlbumPhotosByArea(photos []AlbumPhoto) {
	sort.SliceStable(photos, func(i, j int) bool {
		return photos[i].Width*photos[i].Height > photos[j].Width*photos[j].Height
	})
}

// isStreetViewPhotoURL reports whether this is a panorama tile rather than a photograph.
func isStreetViewPhotoURL(u string) bool {
	return strings.Contains(strings.ToLower(u), "streetviewpixels-pa.googleapis.com/")
}

// photoURLIdentity strips the size directive, so the same photograph at two sizes is one photo.
func photoURLIdentity(u string) string {
	if idx := strings.LastIndex(u, "="); idx > strings.LastIndex(u, "/") {
		return u[:idx]
	}

	return u
}
