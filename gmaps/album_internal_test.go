package gmaps

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// albumItem builds one entry of the listing: id at [0], kind at [2], url and dimensions under [6].
func albumItem(id, url string, kind, width, height float64) []any {
	item := make([]any, 32)
	item[0] = id
	item[2] = kind
	item[6] = []any{url, "", []any{width, height}}

	return item
}

// albumBody wraps items the way the endpoint does: the anti-hijacking prefix, the photographs at
// index 0 and the place's true total at index 1.
func albumBody(t *testing.T, items []any, total float64) []byte {
	t.Helper()

	payload := []any{items, total, nil, "token", nil}

	encoded, err := json.Marshal(payload)
	require.NoError(t, err)

	return append([]byte(")]}'\n"), encoded...)
}

func TestParseAlbumResponseReadsPhotographsAndTotal(t *testing.T) {
	body := albumBody(t, []any{
		albumItem("CIHM0one", "https://lh3.googleusercontent.com/gps-cs-s/one=w203-h114-k-no", 12, 1599, 899),
		albumItem("CIHM0two", "https://lh3.googleusercontent.com/gps-cs-s/two=w203-h360-k-no", 10, 3024, 4032),
	}, 344)

	photos, total, err := parseAlbumResponse(body)

	require.NoError(t, err)
	require.Equal(t, 344, total)
	require.Len(t, photos, 2)
	require.Equal(t, "CIHM0one", photos[0].ID)
	require.Equal(t, "https://lh3.googleusercontent.com/gps-cs-s/one=w203-h114-k-no", photos[0].URL)
	require.Equal(t, 12, photos[0].Kind)
	require.Equal(t, 1599, photos[0].Width)
	require.Equal(t, 899, photos[0].Height)
	require.Equal(t, 10, photos[1].Kind)
}

// The shape an unauthenticated request answers with: the count is there, the photographs are not.
// It is a fact about the session, not a malformed response, so it must not surface as an error —
// the caller is the one that decides whether to try again through the browser.
func TestParseAlbumResponseTolerantOfATotalWithoutPhotographs(t *testing.T) {
	body := albumBody(t, nil, 344)

	photos, total, err := parseAlbumResponse(body)

	require.NoError(t, err)
	require.Equal(t, 344, total)
	require.Empty(t, photos)
}

func TestParseAlbumResponseSkipsAnItemWithoutAURL(t *testing.T) {
	body := albumBody(t, []any{
		albumItem("CIHM0empty", "", 12, 800, 600),
		albumItem("CIHM0real", "https://lh3.googleusercontent.com/gps-cs-s/real=w203", 12, 800, 600),
	}, 2)

	photos, _, err := parseAlbumResponse(body)

	require.NoError(t, err)
	require.Len(t, photos, 1)
	require.Equal(t, "CIHM0real", photos[0].ID)
}

func TestParseAlbumResponseRefusesSomethingThatIsNotJson(t *testing.T) {
	_, _, err := parseAlbumResponse([]byte(")]}'\n<html>sign in</html>"))

	require.Error(t, err)
}

// Kind 10 is a video and the URL hands back a still frame of it, on the same host a photograph
// comes from. This test used to assert the opposite — that a bigger kind-10 entry was a
// photograph worth ranking below a kind-12 one — which is how a frame of someone's walkthrough
// became storable.
func TestSelectAlbumPhotosDropsVideos(t *testing.T) {
	photos := []AlbumPhoto{
		{ID: "video", URL: "https://lh3.googleusercontent.com/gps-cs-s/a", Kind: albumKindVideo, Width: 1920, Height: 1080},
		{ID: "foto", URL: "https://lh3.googleusercontent.com/gps-cs-s/b", Kind: albumKindUserPhoto, Width: 800, Height: 600},
	}

	selected := selectAlbumPhotos(photos, 10)

	require.Len(t, selected, 1)
	require.Equal(t, "foto", selected[0].ID)
}

func TestSelectAlbumPhotosDropsPanoramas(t *testing.T) {
	photos := []AlbumPhoto{
		{ID: "pano", URL: "https://lh3.googleusercontent.com/gps-cs-s/a", Kind: albumKindPanorama, Width: 7200, Height: 3600},
		{ID: "foto", URL: "https://lh3.googleusercontent.com/gps-cs-s/b", Kind: albumKindUserPhoto, Width: 800, Height: 600},
	}

	selected := selectAlbumPhotos(photos, 10)

	require.Len(t, selected, 1)
	require.Equal(t, "foto", selected[0].ID)
}

// A thin album is not a problem to be solved with worse material: four good photographs beat four
// plus six frames of a video, and the old top-up said otherwise.
func TestSelectAlbumPhotosLeavesAThinAlbumThin(t *testing.T) {
	photos := []AlbumPhoto{
		{ID: "a", URL: "https://lh3.googleusercontent.com/gps-cs-s/a", Kind: albumKindVideo, Width: 1920, Height: 1080},
		{ID: "b", URL: "https://lh3.googleusercontent.com/gps-cs-s/b", Kind: 1, Width: 800, Height: 600},
		{ID: "c", URL: "https://lh3.googleusercontent.com/gps-cs-s/c", Kind: albumKindPanorama, Width: 7200, Height: 3600},
	}

	require.Empty(t, selectAlbumPhotos(photos, 10))
}

// The kind is the first gate, not the only one: a kind we have not met yet must not be able to
// walk an equirectangular strip or a baked-in camera angle past us.
func TestSelectAlbumPhotosDropsPanoramaShapeAndAngleWhateverTheKind(t *testing.T) {
	photos := []AlbumPhoto{
		{ID: "forma", URL: "https://lh3.googleusercontent.com/gps-cs-s/a", Kind: albumKindUserPhoto, Width: 7200, Height: 3600},
		{ID: "angulo", URL: "https://lh3.googleusercontent.com/gps-cs-s/b=w224-h298-k-no-pi0.03-ya80.00-ro1.02-fo100", Kind: albumKindUserPhoto, Width: 800, Height: 600},
		{ID: "foto", URL: "https://lh3.googleusercontent.com/gps-cs-s/c=s4096", Kind: albumKindUserPhoto, Width: 800, Height: 600},
	}

	selected := selectAlbumPhotos(photos, 10)

	require.Len(t, selected, 1)
	require.Equal(t, "foto", selected[0].ID)
}

func TestPanoramaHelpersReadTheShapeAndTheDirective(t *testing.T) {
	require.True(t, isPanoramaShape(7200, 3600))
	require.True(t, isPanoramaShape(4096, 2048))
	require.False(t, isPanoramaShape(1920, 1080))
	require.False(t, isPanoramaShape(0, 0))

	require.True(t, hasPanoramaDirective("https://lh3.googleusercontent.com/p/x=w224-h298-k-no-pi0-ya80-ro1-fo100"))
	// A plain directive must not look like an angle: -rw is WebP and -no is "no overlay".
	require.False(t, hasPanoramaDirective("https://lh3.googleusercontent.com/p/x=s4096-rw-l60"))
	require.False(t, hasPanoramaDirective("https://lh3.googleusercontent.com/p/x=w224-h298-k-no"))
	require.False(t, hasPanoramaDirective("https://lh3.googleusercontent.com/p/x"))
}

func TestSelectAlbumPhotosKeepsTheBiggestFirst(t *testing.T) {
	photos := []AlbumPhoto{
		{ID: "small", URL: "https://lh3.googleusercontent.com/gps-cs-s/small", Kind: 12, Width: 800, Height: 600},
		{ID: "huge", URL: "https://lh3.googleusercontent.com/gps-cs-s/huge", Kind: 12, Width: 4032, Height: 3024},
		{ID: "mid", URL: "https://lh3.googleusercontent.com/gps-cs-s/mid", Kind: 12, Width: 1599, Height: 899},
	}

	selected := selectAlbumPhotos(photos, 3)

	require.Equal(t, []string{"huge", "mid", "small"}, []string{selected[0].ID, selected[1].ID, selected[2].ID})
}

// Kept because every one of the 1239 Street View tiles a past sweep collected answered 403: a
// queued photograph that can never download is worse than no photograph at all.
func TestSelectAlbumPhotosDropsStreetViewTiles(t *testing.T) {
	photos := []AlbumPhoto{
		{ID: "tile", URL: "https://streetviewpixels-pa.googleapis.com/v1/thumbnail?panoid=X&w=203", Kind: 12, Width: 203, Height: 100},
		{ID: "real", URL: "https://lh3.googleusercontent.com/gps-cs-s/real", Kind: 12, Width: 800, Height: 600},
	}

	selected := selectAlbumPhotos(photos, 10)

	require.Len(t, selected, 1)
	require.Equal(t, "real", selected[0].ID)
}

func TestSelectAlbumPhotosDedupesTheSamePhotographTwice(t *testing.T) {
	photos := []AlbumPhoto{
		{ID: "same", URL: "https://lh3.googleusercontent.com/gps-cs-s/x=w203-h114-k-no", Kind: 12, Width: 203, Height: 114},
		{ID: "same", URL: "https://lh3.googleusercontent.com/gps-cs-s/x=w800-h600-k-no", Kind: 12, Width: 800, Height: 600},
	}

	require.Len(t, selectAlbumPhotos(photos, 10), 1)
}

// Without an id to key on, the size directive is the only difference between two addresses of one
// photograph — so the identity has to ignore it.
func TestSelectAlbumPhotosDedupesByUrlWhenTheIdIsMissing(t *testing.T) {
	photos := []AlbumPhoto{
		{URL: "https://lh3.googleusercontent.com/gps-cs-s/y=w203-h114-k-no", Kind: 12, Width: 203, Height: 114},
		{URL: "https://lh3.googleusercontent.com/gps-cs-s/y=s4096", Kind: 12, Width: 4000, Height: 3000},
	}

	require.Len(t, selectAlbumPhotos(photos, 10), 1)
}

func TestSelectAlbumPhotosRespectsTheLimit(t *testing.T) {
	photos := make([]AlbumPhoto, 0, 30)
	for i := 0; i < 30; i++ {
		photos = append(photos, AlbumPhoto{
			ID:    string(rune('a' + i)),
			URL:   "https://lh3.googleusercontent.com/gps-cs-s/" + string(rune('a'+i)),
			Kind:  12,
			Width: 800, Height: 600,
		})
	}

	require.Len(t, selectAlbumPhotos(photos, 10), 10)
	require.Empty(t, selectAlbumPhotos(photos, 0))
}

func TestAlbumURLCarriesTheFeatureIdAndLanguage(t *testing.T) {
	u := albumURL("0x935a3002feee27b1:0x7bf9e264080130b4", "pt", "br")

	require.Contains(t, u, "/maps/rpc/photo/listentityphotos")
	require.Contains(t, u, "hl=pt")
	require.Contains(t, u, "gl=br")
	require.Contains(t, u, "0x935a3002feee27b1%3A0x7bf9e264080130b4")
}

func TestAlbumURLFallsBackToAnEnglishLocale(t *testing.T) {
	u := albumURL("0xabc:0xdef", "", "")

	require.Contains(t, u, "hl=en")
	require.Contains(t, u, "gl=us")
}

func TestAlbumPhotoLimitIsOffUntilAskedFor(t *testing.T) {
	t.Setenv("GMAPS_ALBUM_PHOTO_LIMIT", "")
	require.Equal(t, 0, albumPhotoLimit())

	t.Setenv("GMAPS_ALBUM_PHOTO_LIMIT", "24")
	require.Equal(t, 24, albumPhotoLimit())

	// A value past the ceiling is clamped rather than honoured: the album runs inside the page,
	// and the page is the part of the scrape that hangs.
	t.Setenv("GMAPS_ALBUM_PHOTO_LIMIT", "500")
	require.Equal(t, albumMaxLimit, albumPhotoLimit())

	for _, bad := range []string{"abc", "-3", "1.5"} {
		t.Setenv("GMAPS_ALBUM_PHOTO_LIMIT", bad)
		require.Equal(t, 0, albumPhotoLimit())
	}
}

// Documents the measurement that decided the design: without a browser page there is nothing to
// fetch with, and the caller must be told rather than handed an empty album it would read as
// "this place has no photographs".
func TestFetchAlbumRefusesWithoutABrowserPage(t *testing.T) {
	photos, err := FetchAlbum(t.Context(), nil, "0xabc:0xdef", "pt", "br", 10)

	require.Error(t, err)
	require.Empty(t, photos)
}

func TestFetchAlbumIsANoOpWhenDisabledOrUnidentified(t *testing.T) {
	photos, err := FetchAlbum(t.Context(), nil, "0xabc:0xdef", "pt", "br", 0)
	require.NoError(t, err)
	require.Empty(t, photos)

	photos, err = FetchAlbum(t.Context(), nil, "", "pt", "br", 10)
	require.NoError(t, err)
	require.Empty(t, photos)
}
