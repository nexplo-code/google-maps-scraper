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
	// The ceiling on any value asked for. The limit is a cap on what is *carried out* of a listing
	// already fetched, never on what is fetched, so it costs no page time whatever it is set to.
	//
	// Ten is the right value in production and there is no point carrying more: the storage budget
	// on the other side is ten per place, the Node selection cuts at ten, and a download that
	// fails is never refilled from the leftovers — so a surplus is bytes in a CSV that nothing
	// reads. It used to be justified as covering "the kinds that get filtered", which stopped
	// being true when the filtering moved ahead of the cut.
	albumMaxLimit = 60

	/*
		Photo kinds, as the entity array reports them at index 2. Measured, not guessed — against
		one tourist place whose album holds 1466 entries, of the 150 the listing returns:

		  12  103  a photograph, aspect ratios all over the place
		  10   37  a VIDEO. The entry carries the literal string "Video", and every aspect ratio is
		           0.56 or 1.78 — portrait and landscape video. What index [6][0] hands back is the
		           still frame, on the same lh3 host as a photograph.
		  11   10  a PANORAMA. Every single one is exactly 2:1 equirectangular, e.g. 7200x3600.

		This file previously read 10 as "some other kind of photograph" and topped up with it when
		a place was short of photographs — which quietly made videos and panoramas storable.
	*/
	albumKindUserPhoto = 12
	albumKindVideo     = 10
	albumKindPanorama  = 11

	// A panorama is 2:1, give or take the odd stitched pixel. Belt and braces behind the kind:
	// a kind we have not met yet cannot sneak an equirectangular strip past us on aspect alone.
	albumPanoramaAspectMin = 1.97
	albumPanoramaAspectMax = 2.03

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
selectAlbumPhotos keeps the photographs worth storing, biggest first.

An allowlist, not a filter: only kind 12 survives, and anything else is dropped however short of
the limit the place leaves us. This used to be a preference with a top-up — "a place whose whole
album is five panoramas must still come back with five" — and that reasoning was wrong twice over.
It assumed the other kinds were photographs (10 is a video), and it treated a thin album as a
problem worth solving with worse material. A panorama is not a photograph of the place and a still
frame lifted out of a video is not one either; a place with four good photographs is better served
by four than by four plus six frames of someone's walkthrough.

Then the guards that do not depend on the kind, because an unmet kind must not be able to walk
past: no Street View tile (403 to anything but Maps itself — 1239 of them in one past sweep, which
is what the unexplained "403 problem" in the run notes actually was), no baked-in camera angle, no
2:1 equirectangular.

Biggest first, by pixel area, because whoever stores these rewrites the size directive to ask for
the original and that request never upscales — so native resolution decides what the stored
photograph will actually look like.

All of this is arithmetic on an answer already in hand: no extra request, no extra page time.
*/
func selectAlbumPhotos(photos []AlbumPhoto, limit int) []AlbumPhoto {
	if limit <= 0 {
		return nil
	}

	selected := make([]AlbumPhoto, 0, len(photos))
	seen := make(map[string]struct{}, len(photos))

	for _, photo := range photos {
		if photo.URL == "" || photo.Kind != albumKindUserPhoto {
			continue
		}

		if !isStorablePhotoURL(photo.URL) || isPanoramaShape(photo.Width, photo.Height) {
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

		selected = append(selected, photo)
	}

	sortAlbumPhotosByArea(selected)

	if len(selected) > limit {
		selected = selected[:limit]
	}

	return selected
}

// isStorablePhotoURL rejects the two things a URL can tell us are not a plain photograph.
func isStorablePhotoURL(u string) bool {
	return !isStreetViewPhotoURL(u) && !hasPanoramaDirective(u)
}

/*
hasPanoramaDirective reports whether the size directive carries a camera angle.

A photosphere is served from the same lh3 host as a photograph and is given a viewing angle in its
directive — `=w224-h298-k-no-pi0.03-ya80.00-ro1.02-fo100`: pitch, yaw, roll, field of view. Those
only mean something for imagery you can turn around in.

This matters twice. The entry is not a photograph of the place, and the crop is not even stable:
asking for a different size replaces the whole directive, so `=s4096-rw-l60` throws the angle away
and returns the entire equirectangular strip. 161 of these are already stored in our catalogue,
26 of them as a place's cover.
*/
func hasPanoramaDirective(u string) bool {
	directive := u
	if idx := strings.LastIndex(u, "="); idx > strings.LastIndex(u, "/") {
		directive = u[idx:]
	} else {
		return false
	}

	lowered := strings.ToLower(directive)

	return strings.Contains(lowered, "-ya") || strings.Contains(lowered, "-pi") ||
		strings.Contains(lowered, "-ro") || strings.Contains(lowered, "-fo")
}

// isPanoramaShape reports whether these dimensions are equirectangular.
func isPanoramaShape(width, height int) bool {
	if width <= 0 || height <= 0 {
		return false
	}

	aspect := float64(width) / float64(height)

	return aspect >= albumPanoramaAspectMin && aspect <= albumPanoramaAspectMax
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
