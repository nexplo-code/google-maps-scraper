package gmaps_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/gosom/google-maps-scraper/gmaps"
)

func TestNewGmapJobBuildsURLFromQuery(t *testing.T) {
	tests := []struct {
		name        string
		query       string
		expectedURL string
	}{
		{
			name:        "regular search query",
			query:       "pizza in NYC",
			expectedURL: "https://www.google.com/maps/search/pizza+in+NYC",
		},
		{
			name:        "search URL with maps path",
			query:       "https://www.google.com/maps/search/pizza",
			expectedURL: "https://www.google.com/maps/search/pizza?hl=en",
		},
		{
			name:        "place URL",
			query:       "https://www.google.com/maps/place/Empire+State+Building/@40.7484405,-73.9856632",
			expectedURL: "https://www.google.com/maps/place/Empire+State+Building/@40.7484405,-73.9856632?hl=en",
		},
		{
			name:        "short URL",
			query:       "maps.app.goo.gl/abc123",
			expectedURL: "maps.app.goo.gl/abc123?hl=en",
		},
		{
			name:        "google.com/maps without scheme",
			query:       "google.com/maps/search/pizza",
			expectedURL: "https://www.google.com/maps/search/google.com%2Fmaps%2Fsearch%2Fpizza",
		},
		{
			name:        "maps.google.com URL",
			query:       "https://maps.google.com/maps?z=16&q=Empire+State+Building",
			expectedURL: "https://maps.google.com/maps?hl=en&q=Empire+State+Building&z=16",
		},
		{
			name:        "empty string",
			query:       "",
			expectedURL: "https://www.google.com/maps/search/",
		},
		{
			name:        "invalid URL",
			query:       "://invalid",
			expectedURL: "https://www.google.com/maps/search/%3A%2F%2Finvalid",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := gmaps.NewGmapJob("", "en", tt.query, 0, false, "", 0)

			if job.URL != tt.expectedURL {
				t.Errorf("NewGmapJob(..., %q, ...).URL = %q, want %q", tt.query, job.URL, tt.expectedURL)
			}
		})
	}
}

// A place_id lookup is the only query we can address deterministically, and it is the one shape
// where the language parameter used to break the URL: scrapemate's GetFullURL joins URLParams
// with a literal "?", so "…?q=place_id:<id>" plus hl came out as "…?q=place_id:<id>?hl=pt" and
// Google answered with a page that never loaded the place. GetFullURL is asserted here, not just
// the URL field, because that is the string the browser is actually given.
func TestNewGmapJobKeepsPlaceIDQueryIntact(t *testing.T) {
	const placeID = "ChIJo1ZY7eSlZw0R1jtpU3vh-lM"

	job := gmaps.NewGmapJob("", "pt", "https://www.google.com/maps/place/?q=place_id:"+placeID, 0, false, "", 0)

	full := job.GetFullURL()

	if strings.Count(full, "?") != 1 {
		t.Errorf("GetFullURL() = %q, want a single question mark", full)
	}

	if !strings.Contains(full, "hl=pt") {
		t.Errorf("GetFullURL() = %q, want it to carry hl=pt", full)
	}

	u, err := url.Parse(full)
	if err != nil {
		t.Fatalf("GetFullURL() = %q, which does not parse: %v", full, err)
	}

	if got := u.Query().Get("q"); got != "place_id:"+placeID {
		t.Errorf("q = %q, want %q", got, "place_id:"+placeID)
	}
}
