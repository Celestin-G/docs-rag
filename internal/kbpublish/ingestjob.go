package kbpublish

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
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

const (
	// DescriptionPrefix marks the ingestion jobs started by docs-rag.
	DescriptionPrefix = "docs-rag plan="
	// maxDescriptionLen is the Bedrock limit of a job description.
	maxDescriptionLen = 200
	// RecentJobs is how many of the latest jobs are scanned for docs-rag's own.
	RecentJobs = 50
)

// PlanDescription is the description of the job that ingests a plan.
func PlanDescription(planHash string) string { return DescriptionPrefix + planHash }

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
	Description    string
	StartedAt      time.Time
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
	// Start starts a job with the description (at most 200 characters) and
	// returns ErrIngestConflict when one is running.
	Start(ctx context.Context, description string) (Job, error)
	Get(ctx context.Context, jobID string) (Job, error)
	// Recent returns at most n jobs of the data source, the most recently started
	// first, without failure reasons.
	Recent(ctx context.Context, n int) ([]Job, error)
}

// Syncer runs an ingestion job to the end.
type Syncer struct {
	API IngestAPI
	// Description is set on every job the syncer starts: PlanDescription of the
	// plan being published.
	Description  string
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
		job, err = s.API.Start(ctx, s.Description)
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
// base can still be stale: an earlier run may have written to S3 and crashed, or
// failed to start its job, before ingesting. A sync is started unless the latest
// docs-rag job (found among the RecentJobs latest, by its description prefix) is
//   - COMPLETE with no failed document,
//   - started after newest, the latest LastModified under docs/, and
//   - described as s.Description, i.e. ingested this very plan (this covers a
//     crash after deletions, which leave no LastModified).
//
// A job still running (any, ours or not) is waited for first, then the rules are
// evaluated again. ran is false when nothing was started.
func (s *Syncer) RunIfStale(ctx context.Context, timeout time.Duration, newest time.Time) (job Job, ran bool, err error) {
	ctx, cancel, sleep, interval := s.timed(ctx, timeout)
	defer cancel()
	for {
		jobs, err := s.API.Recent(ctx, RecentJobs)
		if err != nil {
			return Job{}, false, fmt.Errorf("list ingestion jobs: %w", err)
		}
		if i := slices.IndexFunc(jobs, func(j Job) bool { return !j.Terminal() }); i >= 0 {
			running := jobs[i]
			s.logf("ingestion job %s is still %s, waiting for it", running.ID, running.Status)
			if done, err := s.wait(ctx, running, timeout, sleep, interval); err != nil && !done.Terminal() {
				return done, true, err
			}
			continue // its own outcome counts through the re-evaluation
		}
		var ours *Job
		for i := range jobs {
			if strings.HasPrefix(jobs[i].Description, DescriptionPrefix) {
				ours = &jobs[i]
				break
			}
		}
		why := "no docs-rag ingestion job found"
		if ours != nil {
			switch {
			case ours.Status != StatusComplete || ours.Stats.Failed > 0:
				why = fmt.Sprintf("the last docs-rag job %s did not complete cleanly (%s, %d failed documents)", ours.ID, ours.Status, ours.Stats.Failed)
			case !ours.StartedAt.After(newest):
				why = fmt.Sprintf("the last docs-rag job %s started before the newest object of the bucket", ours.ID)
			case ours.Description != s.Description:
				why = fmt.Sprintf("the last docs-rag job %s ingested another plan", ours.ID)
			default:
				return *ours, false, nil
			}
		}
		s.logf("%s: starting a new ingestion job", why)
		job, err = s.run(ctx, timeout, sleep, interval)
		return job, true, err
	}
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
