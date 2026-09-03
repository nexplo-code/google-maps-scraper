package webrunner

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gosom/google-maps-scraper/deduper"
	"github.com/gosom/google-maps-scraper/exiter"
	"github.com/gosom/google-maps-scraper/runner"
	"github.com/gosom/google-maps-scraper/tlmt"
	"github.com/gosom/google-maps-scraper/web"
	"github.com/gosom/google-maps-scraper/web/sqlite"
	"github.com/gosom/scrapemate"
	"github.com/gosom/scrapemate/adapters/writers/csvwriter"
	"github.com/gosom/scrapemate/scrapemateapp"
	"golang.org/x/sync/errgroup"
)

// How long to wait for Start to return after its own context has already expired. Past this the
// scrape is provably over and only the browser teardown can still be running.
const teardownGrace = 90 * time.Second

type webrunner struct {
	srv       *web.Server
	svc       *web.Service
	cfg       *runner.Config
	setupMate func(context.Context, io.Writer, *web.Job) (mateRunner, error)
}

type mateRunner interface {
	Start(context.Context, ...scrapemate.IJob) error
	Close() error
}

func New(cfg *runner.Config) (runner.Runner, error) {
	if cfg.DataFolder == "" {
		return nil, fmt.Errorf("data folder is required")
	}

	if err := os.MkdirAll(cfg.DataFolder, os.ModePerm); err != nil {
		return nil, err
	}

	const dbfname = "jobs.db"

	dbpath := filepath.Join(cfg.DataFolder, dbfname)

	repo, err := sqlite.New(dbpath)
	if err != nil {
		return nil, err
	}

	svc := web.NewService(repo, cfg.DataFolder)

	srv, err := web.New(svc, cfg.Addr)
	if err != nil {
		return nil, err
	}

	ans := webrunner{
		srv:       srv,
		svc:       svc,
		cfg:       cfg,
		setupMate: defaultSetupMate(cfg),
	}

	return &ans, nil
}

func (w *webrunner) Run(ctx context.Context) error {
	egroup, ctx := errgroup.WithContext(ctx)

	egroup.Go(func() error {
		return w.work(ctx)
	})

	egroup.Go(func() error {
		return w.srv.Start(ctx)
	})

	return egroup.Wait()
}

func (w *webrunner) Close(context.Context) error {
	return nil
}

func (w *webrunner) work(ctx context.Context) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			jobs, err := w.svc.SelectPending(ctx)
			if err != nil {
				return err
			}

			for i := range jobs {
				select {
				case <-ctx.Done():
					return nil
				default:
					t0 := time.Now().UTC()
					if err := w.scrapeJob(ctx, &jobs[i]); err != nil {
						params := map[string]any{
							"job_count": len(jobs[i].Data.Keywords),
							"duration":  time.Now().UTC().Sub(t0).String(),
							"error":     err.Error(),
						}

						evt := tlmt.NewEvent("web_runner", params)

						_ = runner.Telemetry().Send(ctx, evt)

						log.Printf("error scraping job %s: %v", jobs[i].ID, err)
					} else {
						params := map[string]any{
							"job_count": len(jobs[i].Data.Keywords),
							"duration":  time.Now().UTC().Sub(t0).String(),
						}

						_ = runner.Telemetry().Send(ctx, tlmt.NewEvent("web_runner", params))

						log.Printf("job %s scraped successfully", jobs[i].ID)
					}
				}
			}
		}
	}
}

func (w *webrunner) scrapeJob(ctx context.Context, job *web.Job) error {
	job.Status = web.StatusWorking

	err := w.svc.Update(ctx, job)
	if err != nil {
		return err
	}

	if len(job.Data.Keywords) == 0 {
		job.Status = web.StatusFailed

		return w.svc.Update(ctx, job)
	}

	outpath := filepath.Join(w.cfg.DataFolder, job.ID+".csv")

	outfile, err := os.Create(outpath)
	if err != nil {
		return err
	}

	defer func() {
		_ = outfile.Close()
	}()

	setupMate := w.setupMate
	if setupMate == nil {
		setupMate = defaultSetupMate(w.cfg)
	}

	mate, err := setupMate(ctx, outfile, job)
	if err != nil {
		job.Status = web.StatusFailed

		err2 := w.svc.Update(ctx, job)
		if err2 != nil {
			log.Printf("failed to update job status: %v", err2)
		}

		return err
	}

	defer mate.Close()

	var coords string
	if job.Data.Lat != "" && job.Data.Lon != "" {
		coords = job.Data.Lat + "," + job.Data.Lon
	}

	dedup := deduper.New()
	exitMonitor := exiter.New()

	seedJobs, err := runner.CreateSeedJobs(
		job.Data.FastMode,
		job.Data.Lang,
		strings.NewReader(strings.Join(job.Data.Keywords, "\n")),
		job.Data.Depth,
		job.Data.Email,
		coords,
		job.Data.Zoom,
		func() float64 {
			if job.Data.Radius <= 0 {
				return 10000 // 10 km
			}

			return float64(job.Data.Radius)
		}(),
		dedup,
		exitMonitor,
		w.cfg.ExtraReviews || job.Data.ExtraReviews,
	)
	if err != nil {
		err2 := w.svc.Update(ctx, job)
		if err2 != nil {
			log.Printf("failed to update job status: %v", err2)
		}

		return err
	}

	if len(seedJobs) > 0 {
		exitMonitor.SetSeedCount(len(seedJobs))

		allowedSeconds := max(60, len(seedJobs)*10*job.Data.Depth/50+120)

		if job.Data.MaxTime > 0 {
			if job.Data.MaxTime.Seconds() < 180 {
				allowedSeconds = 180
			} else {
				allowedSeconds = int(job.Data.MaxTime.Seconds())
			}
		}

		log.Printf("running job %s with %d seed jobs and %d allowed seconds", job.ID, len(seedJobs), allowedSeconds)

		mateCtx, cancel := context.WithTimeout(ctx, time.Duration(allowedSeconds)*time.Second)
		defer cancel()

		exitMonitor.SetCancelFunc(cancel)

		go exitMonitor.Run(mateCtx)

		// Start is waited on with a deadline of its own, because it can outlive the scrape.
		//
		// When the browser stops answering the Playwright protocol, `browserContext.Close()`
		// blocks on a select with no timeout — inside scrapemateapp's own shutdown defer, where
		// no context reaches it. A goroutine dump from a stuck container showed exactly that,
		// with no worker goroutines left: the crawl had finished and only the teardown hung.
		// It happened 37 times in 38 regions, each costing an operator ten minutes.
		//
		// mateCtx bounds the scraping itself, so once it is done — by deadline, by the exit
		// monitor noticing the crawl went quiet, or by the parent being cancelled — whatever
		// Start is still doing is not scraping. The results are already in the CSV at that
		// point, so the job is complete and the queue should move on.
		//
		// The grace is timed from mateCtx being done rather than from the job's full allowance,
		// which is what a first version got wrong. A region that finished crawling in 78 seconds
		// under a 720-second allowance still waited the whole 720 plus the grace before anyone
		// noticed the hang: twelve minutes of a worker holding a queue slot to wait for nothing.
		startDone := make(chan error, 1)

		go func() {
			startDone <- mate.Start(mateCtx, seedJobs...)
		}()

		handleStartErr := func(startErr error) error {
			if startErr != nil && !errors.Is(startErr, context.DeadlineExceeded) &&
				!errors.Is(startErr, context.Canceled) {
				cancel()

				err2 := w.svc.Update(ctx, job)
				if err2 != nil {
					log.Printf("failed to update job status: %v", err2)
				}

				return startErr
			}

			return nil
		}

		select {
		case err = <-startDone:
			if handled := handleStartErr(err); handled != nil {
				return handled
			}
		case <-mateCtx.Done():
			// Scraping is over. Give the shutdown a bounded chance to finish on its own before
			// walking away from it.
			select {
			case err = <-startDone:
				if handled := handleStartErr(err); handled != nil {
					return handled
				}
			case <-time.After(teardownGrace):
				// The goroutine and its browser are abandoned rather than reclaimed: there is
				// no way to interrupt a call blocked inside the Playwright transport. The
				// container exits on inactivity and is restarted, which collects them.
				log.Printf("job %s: scrape finished but teardown did not return within %s; continuing",
					job.ID, teardownGrace)
			}
		}

		cancel()
	}

	job.Status = web.StatusOK

	return w.svc.Update(ctx, job)
}

func defaultSetupMate(cfg *runner.Config) func(context.Context, io.Writer, *web.Job) (mateRunner, error) {
	return func(_ context.Context, writer io.Writer, job *web.Job) (mateRunner, error) {
		opts := []func(*scrapemateapp.Config) error{
			scrapemateapp.WithConcurrency(cfg.Concurrency),
			scrapemateapp.WithExitOnInactivity(time.Minute * 3),
		}

		if !job.Data.FastMode {
			opts = append(opts,
				scrapemateapp.WithJS(scrapemateapp.DisableImages()),
			)
		} else {
			opts = append(opts,
				scrapemateapp.WithStealth("firefox"),
			)
		}

		opts = runner.AppendBrowserCapacityOptions(opts, cfg)

		hasProxy := false

		if len(cfg.Proxies) > 0 {
			opts = append(opts, scrapemateapp.WithProxies(cfg.Proxies))
			hasProxy = true
		} else if len(job.Data.Proxies) > 0 {
			opts = append(opts,
				scrapemateapp.WithProxies(job.Data.Proxies),
			)
			hasProxy = true
		}

		if !cfg.DisablePageReuse {
			opts = append(opts,
				scrapemateapp.WithPageReuseLimit(2),
				scrapemateapp.WithBrowserReuseLimit(200),
			)
		}

		log.Printf("job %s has proxy: %v", job.ID, hasProxy)

		csvWriter := csvwriter.NewCsvWriter(csv.NewWriter(writer))

		writers := []scrapemate.ResultWriter{csvWriter}

		matecfg, err := scrapemateapp.NewConfig(
			writers,
			opts...,
		)
		if err != nil {
			return nil, err
		}

		return scrapemateapp.NewScrapeMateApp(matecfg)
	}
}
