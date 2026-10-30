package kbpublish

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

type fakeIngest struct {
	startErrs []error // returned in order, then success
	states    []Job   // returned by Start, then Get in order
	starts    int
	gets      int
	getErr    error

	latest    Job // returned by Latest
	latestOK  bool
	latestErr error
}

func (f *fakeIngest) Latest(context.Context) (Job, bool, error) {
	return f.latest, f.latestOK, f.latestErr
}

func (f *fakeIngest) Start(context.Context) (Job, error) {
	f.starts++
	if len(f.startErrs) > 0 {
		err := f.startErrs[0]
		f.startErrs = f.startErrs[1:]
		return Job{}, err
	}
	return f.states[0], nil
}
func (f *fakeIngest) Get(context.Context, string) (Job, error) {
	f.gets++
	if f.getErr != nil {
		return Job{}, f.getErr
	}
	return f.states[min(f.gets, len(f.states)-1)], nil
}

func noSleep(context.Context, time.Duration) error { return nil }

func TestSyncerRun(t *testing.T) {
	boom := errors.New("boom")
	running := Job{ID: "j1", Status: StatusInProgress}
	complete := Job{ID: "j1", Status: StatusComplete, Stats: JobStats{Scanned: 10, NewIndexed: 4}}
	for _, tc := range []struct {
		name    string
		api     *fakeIngest
		wantErr string
		starts  int
		gets    int
	}{
		{"polls until complete", &fakeIngest{states: []Job{{ID: "j1", Status: StatusStarting}, running, running, complete}}, "", 1, 3},
		{"already terminal at start", &fakeIngest{states: []Job{complete}}, "", 1, 0},
		{"retries while another job runs", &fakeIngest{startErrs: []error{ErrIngestConflict, ErrIngestConflict}, states: []Job{complete}}, "", 3, 0},
		{"start error is not retried", &fakeIngest{startErrs: []error{boom}, states: []Job{complete}}, "start ingestion job: boom", 1, 0},
		{"failed documents", &fakeIngest{states: []Job{{ID: "j1", Status: StatusComplete, Stats: JobStats{Failed: 2}, FailureReasons: []string{"bad metadata"}}}}, "2 documents failed: bad metadata", 1, 0},
		{"job failed", &fakeIngest{states: []Job{running, {ID: "j1", Status: StatusFailed, FailureReasons: []string{"no access"}}}}, "ended FAILED: no access", 1, 1},
		{"job stopped", &fakeIngest{states: []Job{running, {ID: "j1", Status: StatusStopped}}}, "ended STOPPED", 1, 1},
		{"get error", &fakeIngest{states: []Job{running}, getErr: boom}, "get ingestion job j1: boom", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Syncer{API: tc.api, Sleep: noSleep}
			job, err := s.Run(t.Context(), time.Hour)
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Errorf("err = %v, want %q", err, tc.wantErr)
			}
			if tc.api.starts != tc.starts || tc.api.gets != tc.gets {
				t.Errorf("starts %d gets %d, want %d %d", tc.api.starts, tc.api.gets, tc.starts, tc.gets)
			}
			if tc.wantErr == "" && job.Status != StatusComplete {
				t.Errorf("status = %s", job.Status)
			}
		})
	}
}

func TestRunIfStale(t *testing.T) {
	boom := errors.New("boom")
	complete := Job{ID: "new", Status: StatusComplete}
	for _, tc := range []struct {
		name    string
		api     *fakeIngest
		ran     bool
		wantErr string
		starts  int
		gets    int
	}{
		{"last job complete: nothing to do", &fakeIngest{latest: Job{ID: "old", Status: StatusComplete}, latestOK: true}, false, "", 0, 0},
		{"no job ever: start one", &fakeIngest{states: []Job{complete}}, true, "", 1, 0},
		{"last job failed: start one", &fakeIngest{latest: Job{ID: "old", Status: StatusFailed}, latestOK: true, states: []Job{complete}}, true, "", 1, 0},
		{"last job stopped: start one", &fakeIngest{latest: Job{ID: "old", Status: StatusStopped}, latestOK: true, states: []Job{complete}}, true, "", 1, 0},
		{"last job completed with failed documents: start one", &fakeIngest{latest: Job{ID: "old", Status: StatusComplete, Stats: JobStats{Failed: 1}}, latestOK: true, states: []Job{complete}}, true, "", 1, 0},
		{"last job running: wait, do not start", &fakeIngest{latest: Job{ID: "old", Status: StatusInProgress}, latestOK: true, states: []Job{{}, {ID: "old", Status: StatusComplete}}}, true, "", 0, 1},
		{"running job then fails", &fakeIngest{latest: Job{ID: "old", Status: StatusStarting}, latestOK: true, states: []Job{{}, {ID: "old", Status: StatusFailed}}}, true, "ended FAILED", 0, 1},
		{"list error", &fakeIngest{latestErr: boom}, false, "list ingestion jobs: boom", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Syncer{API: tc.api, Sleep: noSleep}
			_, ran, err := s.RunIfStale(t.Context(), time.Hour)
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Errorf("err = %v, want %q", err, tc.wantErr)
			}
			if ran != tc.ran || tc.api.starts != tc.starts || tc.api.gets != tc.gets {
				t.Errorf("ran %v starts %d gets %d, want %v %d %d", ran, tc.api.starts, tc.api.gets, tc.ran, tc.starts, tc.gets)
			}
		})
	}
}

func TestSyncerTimeout(t *testing.T) {
	api := &fakeIngest{states: []Job{{ID: "j1", Status: StatusInProgress}}}
	s := &Syncer{API: api, PollInterval: time.Millisecond}
	_, err := s.Run(t.Context(), 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "still IN_PROGRESS") {
		t.Errorf("err = %v", err)
	}
	conflict := &Syncer{API: &fakeIngest{startErrs: slices.Repeat([]error{ErrIngestConflict}, 25), states: []Job{{ID: "x", Status: StatusComplete}}}, PollInterval: 5 * time.Millisecond}
	if _, err := conflict.Run(t.Context(), 20*time.Millisecond); err == nil || !strings.Contains(err.Error(), "waiting for the running ingestion job") {
		t.Errorf("conflict timeout err = %v", err)
	}
}

func TestJobSummary(t *testing.T) {
	s := Job{ID: "j", Status: StatusComplete, Stats: JobStats{Scanned: 5, Failed: 1}, FailureReasons: []string{"why"}}.Summary()
	for _, want := range []string{"Ingestion job j: COMPLETE", "scanned 5", "failed 1", "failure: why"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary lacks %q: %s", want, s)
		}
	}
}

func TestCheckRegion(t *testing.T) {
	for r, ok := range map[string]bool{"eu-west-1": true, "eu-central-1": true, "us-east-1": false, "": false, "europe": false} {
		if got := CheckRegion(r) == nil; got != ok {
			t.Errorf("CheckRegion(%q) ok = %v", r, got)
		}
	}
}
