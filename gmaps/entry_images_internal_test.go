package gmaps

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// newDarray builds a place array long enough to address the gallery slots by index.
func newDarray() []any {
	const size = 260

	return make([]any, size)
}

// galleryItem mimics an Oct 2026 photo entry: a numeric kind at [2] and the URL at [6][0].
func galleryItem(url string, kind float64) []any {
	item := make([]any, 32)
	item[2] = kind
	item[6] = []any{url, "", []any{780.0, 1040.0}}

	return item
}

// legacyGalleryItem mimics the older entry, which nested the URL under [3][0][6][0] and carried
// an attribution label at [2].
func legacyGalleryItem(url, title string) []any {
	item := make([]any, 8)
	item[2] = title
	item[3] = []any{[]any{nil, nil, nil, nil, nil, nil, []any{url}}}

	return item
}

// The header list and the row list are written out by hand, side by side. Adding a column to one
// and not the other does not fail anything — it shifts every value after it by one, and the CSV
// stays perfectly well-formed while meaning something else entirely.
func TestEntryCsvHeadersAndRowStayAligned(t *testing.T) {
	entry := Entry{
		Title:       "Qualquer Lugar",
		Images:      []Image{{Image: "https://lh3.googleusercontent.com/gps-cs-s/a"}},
		AlbumPhotos: []AlbumPhoto{{ID: "x", URL: "https://lh3.googleusercontent.com/gps-cs-s/b"}},
	}

	require.Len(t, entry.CsvRow(), len(entry.CsvHeaders()))
}

func TestGetImagesReadsTheCurrentGallery(t *testing.T) {
	darray := newDarray()
	darray[51] = []any{[]any{
		galleryItem("https://lh3.googleusercontent.com/grass-cs/one", 12),
		galleryItem("https://lh3.googleusercontent.com/grass-cs/two", 10),
	}}

	images := getImages(darray)

	require.Len(t, images, 2)
	require.Equal(t, "https://lh3.googleusercontent.com/grass-cs/one", images[0].Image)
	require.Equal(t, "https://lh3.googleusercontent.com/grass-cs/two", images[1].Image)
}

// The kind at [2] is a number now. The previous reader required a *string* there and silently
// dropped every photo that did not have one, which is how a place came back with a cover and
// nothing else.
func TestGetImagesKeepsPhotosWithoutAnAttributionLabel(t *testing.T) {
	darray := newDarray()
	darray[51] = []any{[]any{galleryItem("https://lh3.googleusercontent.com/grass-cs/solo", 12)}}

	images := getImages(darray)

	require.Len(t, images, 1)
	require.Empty(t, images[0].Title)
}

func TestGetImagesFallsBackToTheOlderLayout(t *testing.T) {
	darray := newDarray()
	darray[51] = nil
	darray[171] = []any{[]any{
		legacyGalleryItem("https://lh5.googleusercontent.com/p/one", "All"),
		legacyGalleryItem("https://lh5.googleusercontent.com/p/two", "Latest"),
	}}

	images := getImages(darray)

	require.Len(t, images, 2)
	require.Equal(t, "All", images[0].Title)
	require.Equal(t, "https://lh5.googleusercontent.com/p/one", images[0].Image)
	require.Equal(t, "Latest", images[1].Title)
}

func TestGetImagesPrefersTheCurrentLayoutWhenBothArePresent(t *testing.T) {
	darray := newDarray()
	darray[51] = []any{[]any{galleryItem("https://lh3.googleusercontent.com/grass-cs/new", 12)}}
	darray[171] = []any{[]any{legacyGalleryItem("https://lh5.googleusercontent.com/p/old", "All")}}

	images := getImages(darray)

	require.Len(t, images, 1)
	require.Equal(t, "https://lh3.googleusercontent.com/grass-cs/new", images[0].Image)
}

func TestGetImagesIsEmptyForAPlaceWithoutPhotos(t *testing.T) {
	require.Empty(t, getImages(newDarray()))
}

func TestGetImagesSkipsAnItemWithoutAURL(t *testing.T) {
	darray := newDarray()
	darray[51] = []any{[]any{
		galleryItem("", 12),
		galleryItem("https://lh3.googleusercontent.com/grass-cs/real", 12),
	}}

	images := getImages(darray)

	require.Len(t, images, 1)
	require.Equal(t, "https://lh3.googleusercontent.com/grass-cs/real", images[0].Image)
}

// Without a title to read, the panorama has to be recognised by its URL.
func TestExtractStreetViewURLFindsThePanoramaWithoutATitle(t *testing.T) {
	images := []Image{
		{Image: "https://lh3.googleusercontent.com/grass-cs/photo"},
		{Image: "https://streetviewpixels-pa.googleapis.com/v1/thumbnail?panoid=ABC123&w=203"},
	}

	require.Equal(t,
		"https://www.google.com/maps/@?api=1&map_action=pano&pano=ABC123",
		extractStreetViewURL(images),
	)
}

func TestExtractStreetViewURLStillReadsTheTitledEntry(t *testing.T) {
	images := []Image{
		{Title: "Street View & 360°", Image: "https://example.com/thumb?panoid=XYZ789&w=100"},
	}

	require.Equal(t,
		"https://www.google.com/maps/@?api=1&map_action=pano&pano=XYZ789",
		extractStreetViewURL(images),
	)
}

func TestExtractStreetViewURLIsEmptyWithoutAPanorama(t *testing.T) {
	require.Empty(t, extractStreetViewURL([]Image{
		{Image: "https://lh3.googleusercontent.com/grass-cs/photo"},
	}))
}
