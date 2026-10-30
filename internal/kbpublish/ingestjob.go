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

// Run starts one ingestion job and waits for it, for at most timeout. A start
// that hits a running job is retried until the timeout. It returns an error when
// the job does not complete, or completes with failed documents.
func (s *Syncer) Run(ctx context.Context, timeout time.Duration) (Job, error) {
	sleep := s.Sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	interval := s.PollInterval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

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

	if err := job.Err(); err != nil {
		return job, err
	}
	return job, nil
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
