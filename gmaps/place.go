package gmaps

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gosom/scrapemate"

	"github.com/gosom/google-maps-scraper/exiter"
)

type PlaceJobOptions func(*PlaceJob)

type PlaceJob struct {
	scrapemate.Job

	UsageInResults          bool
	ExtractEmail            bool
	ExitMonitor             exiter.Exiter
	ExtractExtraReviews     bool
	WriterManagedCompletion bool
}

func NewPlaceJob(parentID, langCode, u string, extractEmail, extraExtraReviews bool, opts ...PlaceJobOptions) *PlaceJob {
	const (
		defaultPrio       = scrapemate.PriorityMedium
		defaultMaxRetries = 3
	)

	job := PlaceJob{
		Job: scrapemate.Job{
			ID:         uuid.New().String(),
			ParentID:   parentID,
			Method:     "GET",
			URL:        u,
			URLParams:  map[string]string{"hl": langCode},
			MaxRetries: defaultMaxRetries,
			Priority:   defaultPrio,
		},
	}

	job.UsageInResults = true
	job.ExtractEmail = extractEmail
	job.ExtractExtraReviews = extraExtraReviews

	for _, opt := range opts {
		opt(&job)
	}

	return &job
}

func WithPlaceJobExitMonitor(exitMonitor exiter.Exiter) PlaceJobOptions {
	return func(j *PlaceJob) {
		j.ExitMonitor = exitMonitor
	}
}

func WithPlaceJobWriterManagedCompletion() PlaceJobOptions {
	return func(j *PlaceJob) {
		j.WriterManagedCompletion = true
	}
}

func (j *PlaceJob) ProcessOnFetchError() bool {
	return true
}

func (j *PlaceJob) Process(_ context.Context, resp *scrapemate.Response) (any, []scrapemate.IJob, error) {
	defer func() {
		resp.Document = nil
		resp.Body = nil
		resp.Meta = nil
	}()

	if resp.Error != nil {
		if j.ExitMonitor != nil {
			j.ExitMonitor.IncrPlacesCompleted(1)
		}

		return nil, nil, resp.Error
	}

	raw, ok := resp.Meta["json"].([]byte)
	if !ok {
		if j.ExitMonitor != nil {
			j.ExitMonitor.IncrPlacesCompleted(1)
		}

		return nil, nil, fmt.Errorf("could not convert to []byte")
	}

	entry, err := EntryFromJSON(raw)
	if err != nil {
		if j.ExitMonitor != nil {
			j.ExitMonitor.IncrPlacesCompleted(1)
		}

		return nil, nil, err
	}

	entry.ID = j.ParentID

	if entry.Link == "" {
		entry.Link = j.GetURL()
	}

	// Handle RPC-based reviews
	allReviewsRaw, ok := resp.Meta["reviews_raw"].(FetchReviewsResponse)
	if ok && len(allReviewsRaw.pages) > 0 {
		entry.AddExtraReviews(allReviewsRaw.pages)
	}

	// Handle DOM-based reviews (fallback)
	domReviews, ok := resp.Meta["dom_reviews"].([]DOMReview)
	if ok && len(domReviews) > 0 {
		convertedReviews := ConvertDOMReviewsToReviews(domReviews)

		deduped := dedupeDOMReviewsAgainstPrimary(entry.UserReviews, convertedReviews)
		if len(deduped) != len(convertedReviews) {
			log.Printf("DOM reviews: dropped %d of %d already present in user_reviews",
				len(convertedReviews)-len(deduped), len(convertedReviews))
		}

		entry.UserReviewsExtended = append(entry.UserReviewsExtended, deduped...)
	}

	if j.ExtractEmail && entry.IsWebsiteValidForEmail() {
		opts := []EmailExtractJobOptions{}
		if j.ExitMonitor != nil {
			opts = append(opts, WithEmailJobExitMonitor(j.ExitMonitor))
		}

		if j.WriterManagedCompletion {
			opts = append(opts, WithEmailJobWriterManagedCompletion())
		}

		emailJob := NewEmailJob(j.ID, &entry, opts...)

		j.UsageInResults = false

		return nil, []scrapemate.IJob{emailJob}, nil
	} else if j.ExitMonitor != nil && !j.WriterManagedCompletion {
		j.ExitMonitor.IncrPlacesCompleted(1)
	}

	return &entry, nil, err
}

func (j *PlaceJob) BrowserActions(ctx context.Context, page scrapemate.BrowserPage) scrapemate.Response {
	var resp scrapemate.Response

	pageResponse, err := page.Goto(j.GetURL(), scrapemate.WaitUntilDOMContentLoaded)
	if err != nil {
		resp.Error = err

		return resp
	}

	clickRejectCookiesIfRequired(page)

	const defaultTimeout = 5 * time.Second

	// Ignore WaitForURL errors — Google Maps may redirect slowly especially via proxy
	_ = page.WaitForURL(page.URL(), defaultTimeout)

	resp.URL = pageResponse.URL
	resp.StatusCode = pageResponse.StatusCode
	resp.Headers = pageResponse.Headers

	raw, err := j.extractJSON(page)
	if err != nil {
		resp.Error = err

		return resp
	}

	if resp.Meta == nil {
		resp.Meta = make(map[string]any)
	}

	resp.Meta["json"] = raw

	if j.ExtractExtraReviews {
		reviewCount := j.getReviewCount(raw)
		if reviewCount > 0 { // download reviews for any place that has them
			params := fetchReviewsParams{
				page:        page,
				mapURL:      page.URL(),
				reviewCount: reviewCount,
			}

			// Use the new fallback mechanism that tries RPC first, then DOM
			rpcData, domReviews, err := FetchReviewsWithFallback(ctx, params)

			switch {
			case err != nil:
				fmt.Printf("Warning: review extraction failed: %v\n", err)
			case len(rpcData.pages) > 0:
				resp.Meta["reviews_raw"] = rpcData
			case len(domReviews) > 0:
				resp.Meta["dom_reviews"] = domReviews
			}
		}
	}

	return resp
}

// hasCompletePlaceData reports whether a snapshot of APP_INITIALIZATION_STATE is finished
// hydrating, using the one contradiction the page cannot produce on purpose.
//
// Google Maps fills that state progressively, and the header fields land well before the detail
// ones. A snapshot read too early yields a title, a category, an address, a phone and a rating,
// but a review count of zero — and a rating is only ever shown for a place that has reviews, so
// the pair is impossible. Treating it as a real zero is what made 39% of scraped rows come back
// without their count, their opening hours, or a single review: the count feeds the
// `reviewCount > 0` guard in BrowserActions, so one early read silently cancels review fetching
// for the whole place.
//
// A genuine zero — no rating either — is accepted immediately, so places nobody has reviewed do
// not stall here.
func hasCompletePlaceData(raw []byte) bool {
	entry, err := EntryFromJSON(raw, true)
	if err != nil {
		return false
	}

	return entry.ReviewCount > 0 || entry.ReviewRating == 0
}

// getRaw polls the page until its place data is complete, or the context runs out.
//
// The boolean reports completeness separately from the error so the caller can reload the page
// and try again before settling for a partial snapshot. Returning partial data as success here
// would quietly bypass the reload extractJSON already performs.
func (j *PlaceJob) getRaw(ctx context.Context, page scrapemate.BrowserPage) (any, bool, error) {
	// The last snapshot seen, complete or not, so a page that never hydrates can still yield
	// what the scraper produced before this check existed rather than nothing at all.
	var lastRaw any

	for {
		select {
		case <-ctx.Done():
			if lastRaw != nil {
				return lastRaw, false, nil
			}

			return nil, false, fmt.Errorf("timeout while getting raw data: %w", ctx.Err())
		default:
			raw, err := page.Eval(js)
			if err != nil {
				// Continue retrying on error
				<-time.After(time.Millisecond * 200)
				continue
			}

			// Check for valid non-null result.
			// JS null may arrive as nil, and empty strings are not useful here.
			if raw == nil {
				<-time.After(time.Millisecond * 200)
				continue
			}

			// If it's a string, make sure it's not empty
			if str, ok := raw.(string); ok {
				if str == "" {
					<-time.After(time.Millisecond * 200)
					continue
				}

				lastRaw = raw

				if !hasCompletePlaceData(trimJSONPrefix(str)) {
					<-time.After(time.Millisecond * 200)
					continue
				}
			}

			return raw, true, nil
		}
	}
}

func (j *PlaceJob) extractJSON(page scrapemate.BrowserPage) ([]byte, error) {
	const maxRetries = 2

	for attempt := range maxRetries {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		rawI, complete, err := j.getRaw(ctx, page)

		cancel()

		// A snapshot that never finished hydrating is worth one reload before we settle for it:
		// the place data usually lands on a second, warmer load. Only the last attempt keeps the
		// partial result, so a stubborn page still produces a row.
		if err == nil && !complete && attempt < maxRetries-1 {
			if reloadErr := page.Reload(scrapemate.WaitUntilDOMContentLoaded); reloadErr == nil {
				continue
			}
		}

		if err != nil {
			// On timeout, try reloading the page
			if attempt < maxRetries-1 {
				if reloadErr := page.Reload(scrapemate.WaitUntilDOMContentLoaded); reloadErr == nil {
					continue
				}
			}

			return nil, err
		}

		if rawI == nil {
			if attempt < maxRetries-1 {
				if reloadErr := page.Reload(scrapemate.WaitUntilDOMContentLoaded); reloadErr == nil {
					continue
				}
			}

			return nil, fmt.Errorf("APP_INITIALIZATION_STATE data not found")
		}

		raw, ok := rawI.(string)
		if !ok {
			return nil, fmt.Errorf("could not convert to string, got type %T", rawI)
		}

		return trimJSONPrefix(raw), nil
	}

	return nil, fmt.Errorf("APP_INITIALIZATION_STATE data not found after retries")
}

// trimJSONPrefix strips the anti-hijacking prefix Google puts in front of the state blob.
func trimJSONPrefix(raw string) []byte {
	const prefix = `)]}'`

	return []byte(strings.TrimSpace(strings.TrimPrefix(raw, prefix)))
}

func (j *PlaceJob) getReviewCount(data []byte) int {
	tmpEntry, err := EntryFromJSON(data, true)
	if err != nil {
		return 0
	}

	return tmpEntry.ReviewCount
}

func (j *PlaceJob) UseInResults() bool {
	return j.UsageInResults
}

const js = `
(function() {
	if (!window.APP_INITIALIZATION_STATE || !window.APP_INITIALIZATION_STATE[3]) {
		return null;
	}
	const appState = window.APP_INITIALIZATION_STATE[3];
	
	// Search all properties of appState for arrays containing JSON strings
	for (const key of Object.keys(appState)) {
		const arr = appState[key];
		if (Array.isArray(arr)) {
			// Check indices 6 and 5 (where place data typically is)
			for (const idx of [6, 5]) {
				const item = arr[idx];
				if (typeof item === 'string' && item.startsWith(")]}'")) {
					return item;
				}
			}
		}
	}
	return null;
})()
`
