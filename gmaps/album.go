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
	// How much of an unknown envelope element the shape diagnostic prints before cutting it.
	albumShapeSampleChars = 220

	// Where the envelope carries the next page's token. Measured 2026-11-02 against a place with
	// 3699 photographs: 16 elements, index 5 a base64 token, index 3 a per-request id.
	albumNextTokenIndex = 5

	/*
		How many pages one place may cost.

		The listing serves ten at a time, so a limit of thirty needs four pages once videos and
		panoramas are spent on the way. The cap is what keeps a place with 3699 photographs from
		turning one scrape into 370 in-page requests: the album is a bonus on a page whose shutdown
		is the fragile part of the scrape, and `limit` — not completeness — is what we owe.
	*/
	albumMaxPages = 12

	// What the page itself asks the Wiz endpoint for in one call, copied from the capture.
	albumWizPageSize = 20

	// The rpc the Maps page uses for a place and its photographs.
	albumWizRPCID = "hspqX"

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
func albumURL(ftid, lang, gl, pageToken string) string {
	if lang == "" {
		lang = "en"
	}

	if gl == "" {
		gl = "us"
	}

	/*
	   The empty `!5s` was never empty by design — it is the page token, and leaving it blank asks
	   for page one every time.

	   Measured 2026-11-02: the listing serves exactly ten photographs per page and reports the
	   place's real total beside them (3699 for one restaurant, 2645 for another), and the envelope
	   carries the next page's token at index 5. So a blank token is not "give me everything", it
	   is "give me the first ten" — which is why a sweep that once gathered thirty candidates per
	   place came back with eight, with nothing in this file having changed.
	*/
	pb := fmt.Sprintf(
		"!1e2!3m3!1s%s!2s%s!5s%s!6m1!1e1",
		url.QueryEscape(ftid), lang, url.QueryEscape(pageToken),
	)

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

	/*
	   One page is ten photographs; the limit decides how many pages we buy.

	   Each page is a fetch inside a page that is already open, so a page costs a request and no
	   page time — but it is not free, and a place with 3699 photographs would be 370 of them. The
	   loop stops at whichever comes first: enough kept for the limit, no next token, or the page
	   cap. The selection runs once at the end, over everything gathered, because it sorts by pixel
	   area and a per-page sort would hand back the biggest of page one rather than of the album.
	*/
	var (
		photos    []AlbumPhoto
		total     int
		pageToken string
		pages     int
	)

	for pages < albumMaxPages {
		batch, pageTotal, next, err := fetchAndParseAlbum(ctx, page, ftid, lang, gl, pageToken)
		if err != nil {
			// A page that fails after others succeeded is not a failure of the album: the place
			// keeps what the earlier pages gave it. Only the first page can fail the call.
			if pages == 0 {
				return nil, err
			}

			break
		}

		pages++

		photos = append(photos, batch...)

		if pageTotal > total {
			total = pageTotal
		}

		/*
		   Counted on what survives the guards, not on what arrived.

		   A page of ten videos keeps nothing, and stopping on arrivals would end the walk with an
		   empty album while the photographs sat on page two.
		*/
		if len(selectAlbumPhotos(photos, limit)) >= limit || next == "" || next == pageToken {
			break
		}

		pageToken = next
	}

	selected := selectAlbumPhotos(photos, limit)

	/*
	   Asked again when the first answer contradicts itself.

	   The listing reports the place's true photo count even when it withholds the photographs, so
	   "no photographs, but 5 exist" is a refusal and "no photographs, and none exist" is a fact.
	   Observed on consecutive runs over the same four places: the same place answered with 4
	   photographs, then with 1, then with none, while its stated total never moved — the request
	   races the page's own session, and asking again on a page that is already open costs one
	   request and no page time.

	   One retry, not a loop, and from page one: if the second answer is as thin as the first, the
	   place keeps the photographs the page itself gave us.
	*/
	if len(selected) == 0 && total > 0 {
		time.Sleep(albumRetryDelay)

		retried, retriedTotal, _, retryErr := fetchAndParseAlbum(ctx, page, ftid, lang, gl, "")
		if retryErr == nil {
			if again := selectAlbumPhotos(retried, limit); len(again) > 0 {
				return again, nil
			}

			total = retriedTotal
		}
	}

	/*
	   Always accounted for, not only when nothing survives.

	   The old line printed only on a total loss, which left the interesting case silent: a listing
	   that hands back eight photographs where it used to hand back thirty looks like a success from
	   the outside. Whether that is the listing withholding or this file discarding is the whole
	   question, and it is answerable only by printing both sides of the cut — so the counts go out
	   on every call, and `dropped` says which guard spent them.
	*/
	fmt.Printf(
		"album: %s pages=%d listed=%d total=%d kept=%d kinds=%s dropped=%s\n",
		ftid, pages, len(photos), total, len(selected),
		describeAlbumKinds(photos), describeAlbumDrops(photos),
	)

	return selected, nil
}

// describeAlbumKinds counts the listing's entries by the kind reported at index 2.
func describeAlbumKinds(photos []AlbumPhoto) string {
	counts := make(map[int]int, 4)
	for _, photo := range photos {
		counts[photo.Kind]++
	}

	kinds := make([]int, 0, len(counts))
	for kind := range counts {
		kinds = append(kinds, kind)
	}

	sort.Ints(kinds)

	parts := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		parts = append(parts, fmt.Sprintf("%d:%d", kind, counts[kind]))
	}

	if len(parts) == 0 {
		return "none"
	}

	return strings.Join(parts, ",")
}

/*
describeAlbumDrops names the guard that spent each discarded entry.

Mirrors selectAlbumPhotos on purpose rather than sharing a counter with it: the selection runs on
every place and must stay free of bookkeeping, and a diagnostic that drifts from the code it
describes is worse than none. The test holds the two together.
*/
func describeAlbumDrops(photos []AlbumPhoto) string {
	var noURL, wrongKind, notStorable, panorama, duplicate int

	seen := make(map[string]struct{}, len(photos))

	for _, photo := range photos {
		switch {
		case photo.URL == "":
			noURL++
		case photo.Kind != albumKindUserPhoto:
			wrongKind++
		case !isStorablePhotoURL(photo.URL):
			notStorable++
		case isPanoramaShape(photo.Width, photo.Height):
			panorama++
		default:
			key := photo.ID
			if key == "" {
				key = photoURLIdentity(photo.URL)
			}

			if _, done := seen[key]; done {
				duplicate++
				continue
			}

			seen[key] = struct{}{}
		}
	}

	return fmt.Sprintf(
		"no_url=%d wrong_kind=%d not_storable=%d panorama=%d duplicate=%d",
		noURL, wrongKind, notStorable, panorama, duplicate,
	)
}

// fetchAndParseAlbum is one round trip: ask the page, read the answer.
func fetchAndParseAlbum(
	ctx context.Context,
	page scrapemate.BrowserPage,
	ftid, lang, gl, pageToken string,
) ([]AlbumPhoto, int, string, error) {
	if albumUsesWiz() {
		body, err := fetchAlbumViaWiz(ctx, page, ftid, lang, pageToken, albumWizPageSize)
		if err != nil {
			return nil, 0, "", err
		}

		inner, err := unwrapWizEnvelope(body)
		if err != nil {
			return nil, 0, "", err
		}

		return parseAlbumResponse(inner)
	}

	body, err := fetchAlbumWithBrowser(ctx, page, albumURL(ftid, lang, gl, pageToken))
	if err != nil {
		return nil, 0, "", err
	}

	return parseAlbumResponse(body)
}

/*
albumUsesWiz reports whether to walk the album through the page's own endpoint.

On by default: the endpoint it replaces serves ten photographs a page with no way to ask for more
and no token that moves, which is the regression this exists to undo. The variable is the way back
if the Wiz call ever starts failing, not a feature to opt into.
*/
func albumUsesWiz() bool {
	return os.Getenv("GMAPS_ALBUM_SOURCE") != "rpc"
}

/*
unwrapWizEnvelope pulls the rpc payload out of a batchexecute response.

The answer is the anti-hijacking prefix, then length-prefixed chunks, each a JSON array of rows,
and the row we want is `["wrb.fr", "<rpcid>", "<payload as a json string>", ...]` — the payload
arrives as a *string*, encoded a second time. The lengths are not trusted as offsets: a decoder fed
from the current position reads each array and reports where it ended, which survives a chunk whose
declared length disagrees with its content.
*/
func unwrapWizEnvelope(body []byte) ([]byte, error) {
	text := string(body)
	if idx := strings.IndexByte(text, '\n'); strings.HasPrefix(text, ")]}'") && idx >= 0 {
		text = text[idx+1:]
	}

	decoder := json.NewDecoder(strings.NewReader(text))

	for {
		/*
		   Decoded as `any`, not as an array.

		   The chunks are separated by their own byte lengths written as bare numbers, so a stream
		   decoder sees `12657`, then `[[...]]`, then `5`, then another array. Decoding straight
		   into a slice turns the first of those lengths into a type error and throws the response
		   away — which is exactly how this failed the first time it ran against four live places.
		*/
		var decoded any

		if err := decoder.Decode(&decoded); err != nil {
			return nil, fmt.Errorf("album wiz envelope has no %s payload", albumWizRPCID)
		}

		chunk, ok := decoded.([]any)
		if !ok {
			continue
		}

		for _, row := range chunk {
			fields, ok := row.([]any)
			if !ok || len(fields) < 3 {
				continue
			}

			if kind, _ := fields[0].(string); kind != "wrb.fr" {
				continue
			}

			if rpc, _ := fields[1].(string); rpc != albumWizRPCID {
				continue
			}

			payload, ok := fields[2].(string)
			if !ok || payload == "" {
				return nil, fmt.Errorf("album wiz payload for %s is empty", albumWizRPCID)
			}

			return []byte(payload), nil
		}
	}
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
func parseAlbumResponse(data []byte) (photos []AlbumPhoto, total int, nextToken string, err error) {
	trimmed := trimJSONPrefix(string(data))

	var parsed []any
	if err := json.Unmarshal(trimmed, &parsed); err != nil {
		return nil, 0, "", fmt.Errorf("album response is not json: %w", err)
	}

	if len(parsed) > 1 {
		if value, ok := parsed[1].(float64); ok {
			total = int(value)
		}
	}

	if len(parsed) == 0 {
		return nil, total, "", nil
	}

	items, ok := parsed[0].([]any)
	if !ok {
		return nil, total, "", nil
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

	describeAlbumEnvelope(parsed)

	// Index 5 is the next page's token; absent or empty means this was the last page.
	nextToken = getNthElementAndCast[string](parsed, albumNextTokenIndex)

	return photos, total, nextToken, nil
}

/*
describeAlbumEnvelope prints the shape of everything the listing returns beside the photographs.

Written to answer one question: the request carries an empty `!5s` where a page token would go, and
the listing serves ten photographs while reporting thousands. If the envelope hands back a token,
the ten are page one and the rest are reachable; if it does not, the page size has to come from the
request. The parser reads indices 0 and 1 and has never looked at the rest, so the answer was never
in a log.

Behind an env var because it prints a slice of a stranger's payload: useful once, noise forever.
*/
func describeAlbumEnvelope(parsed []any) {
	if os.Getenv("GMAPS_ALBUM_DEBUG_SHAPE") != "1" {
		return
	}

	parts := make([]string, 0, len(parsed))

	for i, element := range parsed {
		if i < 2 {
			continue
		}

		encoded, err := json.Marshal(element)
		if err != nil {
			parts = append(parts, fmt.Sprintf("[%d]=unmarshalable", i))
			continue
		}

		text := string(encoded)
		if len(text) > albumShapeSampleChars {
			text = text[:albumShapeSampleChars] + "..."
		}

		parts = append(parts, fmt.Sprintf("[%d]=%s", i, text))
	}

	fmt.Printf("album-shape: elements=%d %s\n", len(parsed), strings.Join(parts, " "))
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
	return !isStreetViewPhotoURL(u) && !isInteriorViewURL(u) && !hasPanoramaDirective(u)
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

/*
isInteriorViewURL reports whether this is Street View imagery from inside the place.

Served from the same lh3 host as a photograph but under `/iv/` — interior view — and it is not
fetchable at all: measured, it answers HTTP 400 with an HTML error both as it comes and with any
size directive appended. So it is useless twice over, as imagery we do not want and as a URL that
can never download. It arrives with no kind at all, which is why the path has to be read.
*/
func isInteriorViewURL(u string) bool {
	return strings.Contains(strings.ToLower(u), "googleusercontent.com/iv/")
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

/*
fetchAlbumViaWiz asks the listing the way the Maps page itself asks it today.

`/maps/rpc/photo/listentityphotos` is the previous generation. It still answers, but it serves ten
photographs a page and the request has no size field we could find — three candidate positions were
tried against the live endpoint on 2026-11-02 and all three were ignored. Meanwhile a HAR of the
page's own album scroll shows it never calls that endpoint at all: it POSTs `hspqX` to the Wiz
batch endpoint, and between page one and page two of the same album exactly one thing changes in
533 bytes of payload — a `null` becomes a continuation token.

So the album is walked here, with the page's own call, and the token is the only thing that moves.

The context block the page sends — a session id and a request counter — goes out as nulls. It is
telemetry the page keeps for itself, and a listing that refused without it would fail loudly on the
first call rather than quietly serve less, which is the failure we are climbing out of.
*/
func fetchAlbumViaWiz(
	_ context.Context,
	page scrapemate.BrowserPage,
	ftid, lang, pageToken string,
	pageSize int,
) ([]byte, error) {
	token := "null"
	if pageToken != "" {
		encoded, err := json.Marshal(pageToken)
		if err != nil {
			return nil, fmt.Errorf("album page token is not encodable: %w", err)
		}

		token = string(encoded)
	}

	ftidJSON, err := json.Marshal(ftid)
	if err != nil {
		return nil, fmt.Errorf("album feature id is not encodable: %w", err)
	}

	/*
	   The payload, copied from the page's own request and reduced to what moves.

	   Kept positionally identical rather than tidied: every null here is a field the page sends as
	   null, and closing the gaps would silently shift the ones that matter — the feature id, the
	   page size and the token.
	*/
	inner := fmt.Sprintf(
		`[2,null,[%s,null,null,null,null,null,null,null,0,null,null,null,null,null,null],null,`+
			`[null,[203,100],[null,%d,%s,null,1],null,null,null,`+
			`[[[1,0,3],[2,1,2],[2,0,3],[8,0,3],[10,0,3],[10,1,2],[10,0,4],[9,1,2]],1],null,0,`+
			`null,null,null,null,null,[[[[[[2]]],[195,195],20]]]],`+
			`null,null,null,null,null,null,null,null,null,null,[null,1,null,1]]`,
		ftidJSON, pageSize, token,
	)

	js := fmt.Sprintf(`async () => {
		try {
			const at = (window.WIZ_global_data && window.WIZ_global_data.SNlM0e) || '';
			const freq = JSON.stringify([[["hspqX", %s, null, "generic"]]]);
			const body = new URLSearchParams();
			body.set('f.req', freq);
			if (at) { body.set('at', at); }
			const url = '/maps/_/MapsWizUi/data/batchexecute'
				+ '?rpcids=hspqX&source-path=/maps&hl=%s&soc-app=162&soc-platform=1&soc-device=1&_reqid=1&rt=c';
			const response = await fetch(url, {
				method: 'POST',
				credentials: 'include',
				headers: { 'Content-Type': 'application/x-www-form-urlencoded;charset=UTF-8' },
				body: body.toString()
			});
			if (!response.ok) { return { error: 'HTTP ' + response.status }; }
			return { data: await response.text(), at: at ? 'sim' : 'nao' };
		} catch (e) {
			return { error: String(e && e.message || e) };
		}
	}`, mustJSONString(inner), lang)

	result, err := page.Eval(js)
	if err != nil {
		return nil, fmt.Errorf("album wiz fetch failed: %w", err)
	}

	resultMap, ok := result.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("album wiz fetch returned %T", result)
	}

	if errMsg, hasError := resultMap["error"]; hasError {
		return nil, fmt.Errorf("album wiz fetch error: %v", errMsg)
	}

	data, ok := resultMap["data"].(string)
	if !ok || len(data) < 10 {
		return nil, fmt.Errorf("album wiz fetch returned an empty body")
	}

	if os.Getenv("GMAPS_ALBUM_DEBUG_SHAPE") == "1" {
		sample := data
		if len(sample) > albumShapeSampleChars {
			sample = sample[:albumShapeSampleChars]
		}

		fmt.Printf("album-wiz: at=%v bytes=%d sample=%q\n", resultMap["at"], len(data), sample)

	}

	return []byte(data), nil
}

// mustJSONString quotes a string for embedding in the JS source built above.
func mustJSONString(s string) string {
	encoded, err := json.Marshal(s)
	if err != nil {
		return `""`
	}

	return string(encoded)
}
