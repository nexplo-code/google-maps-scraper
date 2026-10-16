package gmaps

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/google/uuid"
	"github.com/gosom/scrapemate"

	"github.com/gosom/google-maps-scraper/deduper"
	"github.com/gosom/google-maps-scraper/exiter"
)

type GmapJobOptions func(*GmapJob)

type GmapJob struct {
	scrapemate.Job

	MaxDepth     int
	LangCode     string
	ExtractEmail bool

	Deduper                 deduper.Deduper
	ExitMonitor             exiter.Exiter
	ExtractExtraReviews     bool
	WriterManagedCompletion bool
}

func NewGmapJob(
	id, langCode, query string,
	maxDepth int,
	extractEmail bool,
	geoCoordinates string,
	zoom int,
	opts ...GmapJobOptions,
) *GmapJob {
	var (
		mapURL    string
		urlParams = map[string]string{"hl": langCode}
	)

	switch {
	case isGoogleMapsURL(query):
		// The language goes inside the URL here, not in URLParams.
		//
		// scrapemate's GetFullURL always joins params with a literal "?", so a query string
		// that is already part of the URL — as in /maps/place/?q=place_id:<id> — comes out as
		// "...?q=place_id:<id>?hl=pt". Google answers that with a page that never loads the
		// place, and the job dies on "timeout while getting raw data" a minute later.
		mapURL = withLangCode(strings.TrimSpace(query), langCode)
		urlParams = nil
	case geoCoordinates != "" && zoom > 0:
		query = url.QueryEscape(query)
		mapURL = fmt.Sprintf("https://www.google.com/maps/search/%s/@%s,%dz", query, strings.ReplaceAll(geoCoordinates, " ", ""), zoom)
	default:
		// Warning: geo and zoom MUST be both set or not
		query = url.QueryEscape(query)
		mapURL = fmt.Sprintf("https://www.google.com/maps/search/%s", query)
	}

	const (
		maxRetries = 3
		prio       = scrapemate.PriorityLow
	)

	if id == "" {
		id = uuid.New().String()
	}

	job := GmapJob{
		Job: scrapemate.Job{
			ID:         id,
			Method:     http.MethodGet,
			URL:        mapURL,
			URLParams:  urlParams,
			MaxRetries: maxRetries,
			Priority:   prio,
		},
		MaxDepth:     maxDepth,
		LangCode:     langCode,
		ExtractEmail: extractEmail,
	}

	for _, opt := range opts {
		opt(&job)
	}

	return &job
}

func WithDeduper(d deduper.Deduper) GmapJobOptions {
	return func(j *GmapJob) {
		j.Deduper = d
	}
}

func WithExitMonitor(e exiter.Exiter) GmapJobOptions {
	return func(j *GmapJob) {
		j.ExitMonitor = e
	}
}

func WithExtraReviews() GmapJobOptions {
	return func(j *GmapJob) {
		j.ExtractExtraReviews = true
	}
}

func WithWriterManagedCompletion() GmapJobOptions {
	return func(j *GmapJob) {
		j.WriterManagedCompletion = true
	}
}

func (j *GmapJob) UseInResults() bool {
	return false
}

func (j *GmapJob) ProcessOnFetchError() bool {
	return true
}

func (j *GmapJob) Process(ctx context.Context, resp *scrapemate.Response) (any, []scrapemate.IJob, error) {
	defer func() {
		resp.Document = nil
		resp.Body = nil
	}()

	if resp.Error != nil {
		if j.ExitMonitor != nil {
			j.ExitMonitor.IncrSeedCompleted(1)
		}

		return nil, nil, resp.Error
	}

	log := scrapemate.GetLoggerFromContext(ctx)

	doc, ok := resp.Document.(*goquery.Document)
	if !ok {
		if j.ExitMonitor != nil {
			j.ExitMonitor.IncrSeedCompleted(1)
		}

		return nil, nil, fmt.Errorf("could not convert to goquery document")
	}

	var next []scrapemate.IJob

	if strings.Contains(resp.URL, "/maps/place/") {
		jopts := []PlaceJobOptions{}
		if j.ExitMonitor != nil {
			jopts = append(jopts, WithPlaceJobExitMonitor(j.ExitMonitor))
		}

		if j.WriterManagedCompletion {
			jopts = append(jopts, WithPlaceJobWriterManagedCompletion())
		}

		placeJob := NewPlaceJob(j.ID, j.LangCode, resp.URL, j.ExtractEmail, j.ExtractExtraReviews, jopts...)

		next = append(next, placeJob)
	} else {
		doc.Find(`div[role=feed] div[jsaction]>a`).Each(func(_ int, s *goquery.Selection) {
			if href := s.AttrOr("href", ""); href != "" {
				jopts := []PlaceJobOptions{}
				if j.ExitMonitor != nil {
					jopts = append(jopts, WithPlaceJobExitMonitor(j.ExitMonitor))
				}

				if j.WriterManagedCompletion {
					jopts = append(jopts, WithPlaceJobWriterManagedCompletion())
				}

				nextJob := NewPlaceJob(j.ID, j.LangCode, href, j.ExtractEmail, j.ExtractExtraReviews, jopts...)

				if j.Deduper == nil || j.Deduper.AddIfNotExists(ctx, href) {
					next = append(next, nextJob)
				}
			}
		})
	}

	if j.ExitMonitor != nil {
		j.ExitMonitor.IncrPlacesFound(len(next))
		j.ExitMonitor.IncrSeedCompleted(1)
	}

	log.Info(fmt.Sprintf("%d places found", len(next)))

	return nil, next, nil
}

func (j *GmapJob) BrowserActions(ctx context.Context, page scrapemate.BrowserPage) scrapemate.Response {
	var resp scrapemate.Response

	pageResponse, err := page.Goto(j.GetFullURL(), scrapemate.WaitUntilDOMContentLoaded)
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

	// When Google Maps finds only 1 place, it slowly redirects to that place's URL
	// check element scroll
	sel := `div[role='feed']`

	// A query that addresses one place directly — a /maps/place/ URL, or
	// /maps/place/?q=place_id:<id> — never renders a results feed. Asking for the URL first
	// costs nothing and is what keeps us out of the scroll below: without it the ten second
	// wait for the feed is spent in vain and `scroll` then evaluates `el.scrollHeight` on a
	// null element, which surfaces as a playwright TypeError and fails the whole job.
	singlePlace := strings.Contains(page.URL(), "/maps/place/")

	if !singlePlace {
		err = page.WaitForSelector(sel, 10*time.Second)

		if err != nil {
			waitCtx, waitCancel := context.WithTimeout(ctx, time.Second*5)
			defer waitCancel()

			singlePlace = waitUntilURLContains(waitCtx, page, "/maps/place/")

			waitCancel()
		}
	}

	if singlePlace {
		resp.URL = page.URL()

		var body string

		body, err = page.Content()
		if err != nil {
			resp.Error = err
			return resp
		}

		resp.Body = []byte(body)

		return resp
	}

	scrollSelector := `div[role='feed']`

	_, err = scroll(ctx, page, j.MaxDepth, scrollSelector)
	if err != nil {
		resp.Error = err

		return resp
	}

	body, err := page.Content()
	if err != nil {
		resp.Error = err
		return resp
	}

	resp.Body = []byte(body)

	return resp
}

func waitUntilURLContains(ctx context.Context, page scrapemate.BrowserPage, s string) bool {
	ticker := time.NewTicker(time.Millisecond * 150)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			if strings.Contains(page.URL(), s) {
				return true
			}
		}
	}
}

func clickRejectCookiesIfRequired(page scrapemate.BrowserPage) {
	// Use JavaScript to find and click - faster than multiple locator calls
	_, _ = page.Eval(`() => {
		// Try consent form buttons first
		const consentForm = document.querySelector('form[action*="consent.google"]');
		if (consentForm) {
			const btn = consentForm.querySelector('button, input[type="submit"]');
			if (btn) {
				btn.click();
				return true;
			}
		}
		// Try reject/decline buttons
		const buttons = document.querySelectorAll('button, input[type="submit"]');
		for (const btn of buttons) {
			const text = (btn.textContent || btn.value || '').toLowerCase();
			if (text.includes('reject') || text.includes('decline') || text.includes('ablehnen')) {
				btn.click();
				return true;
			}
		}
		return false;
	}`)
}

func scroll(ctx context.Context,
	page scrapemate.BrowserPage,
	maxDepth int,
	scrollSelector string,
) (int, error) {
	expr := `async () => {
		const el = document.querySelector("` + scrollSelector + `");
		el.scrollTop = el.scrollHeight;

		return new Promise((resolve, reject) => {
  			setTimeout(() => {
    		resolve(el.scrollHeight);
  			}, %d);
		});
	}`

	var currentScrollHeight int
	// Scroll to the bottom of the page.
	waitTime := 100.
	cnt := 0

	const (
		timeout  = 500
		maxWait2 = 2000
	)

	for i := 0; i < maxDepth; i++ {
		cnt++
		waitTime2 := timeout * cnt

		if waitTime2 > timeout {
			waitTime2 = maxWait2
		}

		// Scroll to the bottom of the page.
		scrollHeight, err := page.Eval(fmt.Sprintf(expr, waitTime2))
		if err != nil {
			return cnt, err
		}

		// Handle both int and float64 because browser-evaluated numbers may arrive as either type.
		var height int
		switch v := scrollHeight.(type) {
		case int:
			height = v
		case float64:
			height = int(v)
		default:
			return cnt, fmt.Errorf("scrollHeight is not a number, got %T", scrollHeight)
		}

		if height == currentScrollHeight {
			break
		}

		currentScrollHeight = height

		select {
		case <-ctx.Done():
			return currentScrollHeight, nil
		default:
		}

		waitTime *= 1.5

		if waitTime > maxWait2 {
			waitTime = maxWait2
		}

		page.WaitForTimeout(time.Duration(waitTime) * time.Millisecond)
	}

	return cnt, nil
}

// withLangCode returns rawURL with hl set to langCode, keeping whatever query it already has.
// An unparseable URL is returned untouched: the caller is better served by the original address
// than by a silently mangled one.
func withLangCode(rawURL, langCode string) string {
	if langCode == "" {
		return rawURL
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}

	q := u.Query()
	q.Set("hl", langCode)
	u.RawQuery = q.Encode()

	return u.String()
}

func isGoogleMapsURL(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}

	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		u, err := url.Parse(s)
		if err != nil {
			return false
		}

		host := strings.ToLower(u.Hostname())
		if host == "maps.app.goo.gl" {
			return true
		}

		return (host == "google.com" || strings.HasSuffix(host, ".google.com")) &&
			(strings.Contains(u.EscapedPath(), "/maps") || strings.Contains(u.Path, "/maps"))
	}

	if strings.HasPrefix(s, "maps.app.goo.gl") {
		return true
	}

	return false
}
