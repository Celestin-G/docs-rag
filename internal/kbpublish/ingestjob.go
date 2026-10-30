package kbpublish

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Ingestion job statuses of Bedrock Agent.
const (
	StatusStarting   = "STARTING"
	StatusInProgress = "IN_PROGRESS"
	StatusComplete   = "COMPLETE"
	StatusFailed     = "FAILED"
	StatusStopping   = "STOPPING"
	StatusStopped    = "STOPPED"
)

// ErrIngestConflict means another ingestion job is already running on the data
// source (Bedrock allows one at a time).
var ErrIngestConflict = errors.New("another ingestion job is running")

// JobStats are the counters of an ingestion job.
type JobStats struct {
	Scanned, NewIndexed, Modified, Deleted, Failed, MetadataScanned, MetadataModified int64
}

// Job is the state of one ingestion job.
type Job struct {
	ID             string
	Status         string
	Stats          JobStats
	FailureReasons []string
}

// Terminal reports whether the job will not change anymore.
func (j Job) Terminal() bool {
	switch j.Status {
	case StatusComplete, StatusFailed, StatusStopped:
		return true
	}
	return false
}

// IngestAPI is the subset of the Bedrock Agent API the publisher uses, on the
// configured knowledge base and data source.
type IngestAPI interface {
	// Start starts a job and returns ErrIngestConflict when one is running.
	Start(ctx context.Context) (Job, error)
	Get(ctx context.Context, jobID string) (Job, error)
	// Latest returns the most recently started job of the data source (without
	// failure reasons), false when there is none.
	Latest(ctx context.Context) (Job, bool, error)
}

// Syncer runs an ingestion job to the end.
type Syncer struct {
	API          IngestAPI
	PollInterval time.Duration
	// Sleep waits for d or until ctx is done; replaced in tests.
	Sleep func(ctx context.Context, d time.Duration) error
	Log   io.Writer
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (s *Syncer) logf(format string, args ...any) {
	if s.Log != nil {
		fmt.Fprintf(s.Log, format+"\n", args...)
	}
}

// timed returns the context bounded by timeout, the sleep function and the
// polling interval.
func (s *Syncer) timed(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc, func(context.Context, time.Duration) error, time.Duration) {
	sleep := s.Sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	interval := s.PollInterval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	return ctx, cancel, sleep, interval
}

// Run starts one ingestion job and waits for it, for at most timeout. A start
// that hits a running job is retried until the timeout. It returns an error when
// the job does not complete, or completes with failed documents.
func (s *Syncer) Run(ctx context.Context, timeout time.Duration) (Job, error) {
	ctx, cancel, sleep, interval := s.timed(ctx, timeout)
	defer cancel()
	return s.run(ctx, timeout, sleep, interval)
}

func (s *Syncer) run(ctx context.Context, timeout time.Duration, sleep func(context.Context, time.Duration) error, interval time.Duration) (Job, error) {
	var job Job
	for {
		var err error
		job, err = s.API.Start(ctx)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrIngestConflict) {
			return Job{}, fmt.Errorf("start ingestion job: %w", err)
		}
		s.logf("another ingestion job is running, retrying in %s", interval)
		if err := sleep(ctx, interval); err != nil {
			return Job{}, fmt.Errorf("waiting for the running ingestion job to end: %w", err)
		}
	}
	s.logf("ingestion job %s started", job.ID)
	return s.wait(ctx, job, timeout, sleep, interval)
}

// wait polls the job to a terminal state and returns its error, if any.
func (s *Syncer) wait(ctx context.Context, job Job, timeout time.Duration, sleep func(context.Context, time.Duration) error, interval time.Duration) (Job, error) {
	for !job.Terminal() {
		if err := sleep(ctx, interval); err != nil {
			return job, fmt.Errorf("ingestion job %s still %s after %s: %w", job.ID, job.Status, timeout, err)
		}
		next, err := s.API.Get(ctx, job.ID)
		if err != nil {
			return job, fmt.Errorf("get ingestion job %s: %w", job.ID, err)
		}
		job = next
		s.logf("ingestion job %s: %s", job.ID, job.Status)
	}
	return job, job.Err()
}

// RunIfStale is for a publish that changed nothing in the bucket. The knowledge
// base is still stale when the previous ingestion failed or timed out, so it
// looks at the latest job of the data source: one still running is waited for,
// one that completed with no failed document means nothing to do (ran is
// false), anything else (failed, stopped, failed documents, no job at all)
// starts a new job.
func (s *Syncer) RunIfStale(ctx context.Context, timeout time.Duration) (job Job, ran bool, err error) {
	ctx, cancel, sleep, interval := s.timed(ctx, timeout)
	defer cancel()
	latest, ok, err := s.API.Latest(ctx)
	if err != nil {
		return Job{}, false, fmt.Errorf("list ingestion jobs: %w", err)
	}
	switch {
	case ok && !latest.Terminal():
		s.logf("ingestion job %s is still %s, waiting for it", latest.ID, latest.Status)
		job, err = s.wait(ctx, latest, timeout, sleep, interval)
		return job, true, err
	case ok && latest.Err() == nil:
		return latest, false, nil
	}
	if ok {
		s.logf("the last ingestion job %s did not complete cleanly (%s): starting a new one", latest.ID, latest.Status)
	}
	job, err = s.run(ctx, timeout, sleep, interval)
	return job, true, err
}

// Err is non-nil when the job did not complete cleanly.
func (j Job) Err() error {
	if j.Status != StatusComplete {
		return fmt.Errorf("ingestion job %s ended %s: %s", j.ID, j.Status, strings.Join(j.FailureReasons, "; "))
	}
	if j.Stats.Failed > 0 {
		return fmt.Errorf("ingestion job %s: %d documents failed: %s", j.ID, j.Stats.Failed, strings.Join(j.FailureReasons, "; "))
	}
	return nil
}

// Summary renders the statistics and failure reasons of a job.
func (j Job) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Ingestion job %s: %s\n", j.ID, j.Status)
	fmt.Fprintf(&b, "  scanned %d, new %d, modified %d, deleted %d, failed %d (metadata: scanned %d, modified %d)\n",
		j.Stats.Scanned, j.Stats.NewIndexed, j.Stats.Modified, j.Stats.Deleted, j.Stats.Failed,
		j.Stats.MetadataScanned, j.Stats.MetadataModified)
	for _, r := range j.FailureReasons {
		fmt.Fprintf(&b, "  failure: %s\n", r)
	}
	return b.String()
}
