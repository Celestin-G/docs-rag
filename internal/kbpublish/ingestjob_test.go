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

	recent    []Job // returned by Recent, newest first
	afterWait []Job // returned by Recent once Get was called
	recentErr error
	desc      string // description of the last Start
}

func (f *fakeIngest) Recent(context.Context, int) ([]Job, error) {
	if f.gets > 0 {
		return f.afterWait, f.recentErr
	}
	return f.recent, f.recentErr
}

func (f *fakeIngest) Start(_ context.Context, description string) (Job, error) {
	f.starts++
	f.desc = description
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
	const plan = "docs-rag plan=abc"
	t0 := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	ours := func(mut func(*Job)) []Job {
		j := Job{ID: "old", Status: StatusComplete, Description: plan, StartedAt: t0.Add(time.Hour)}
		if mut != nil {
			mut(&j)
		}
		return []Job{{ID: "other", Status: StatusComplete, Description: "manual sync", StartedAt: t0.Add(2 * time.Hour)}, j}
	}
	for _, tc := range []struct {
		name    string
		api     *fakeIngest
		ran     bool
		wantErr string
		starts  int
		gets    int
	}{
		{"consistent: nothing to do", &fakeIngest{recent: ours(nil)}, false, "", 0, 0},
		{"no job ever: start one", &fakeIngest{states: []Job{complete}}, true, "", 1, 0},
		{"jobs of others only: start one", &fakeIngest{recent: []Job{{ID: "x", Status: StatusComplete, Description: "manual"}}, states: []Job{complete}}, true, "", 1, 0},
		{"last job failed", &fakeIngest{recent: ours(func(j *Job) { j.Status = StatusFailed }), states: []Job{complete}}, true, "", 1, 0},
		{"last job stopped", &fakeIngest{recent: ours(func(j *Job) { j.Status = StatusStopped }), states: []Job{complete}}, true, "", 1, 0},
		{"completed with failed documents", &fakeIngest{recent: ours(func(j *Job) { j.Stats.Failed = 1 }), states: []Job{complete}}, true, "", 1, 0},
		{"started before the newest object", &fakeIngest{recent: ours(func(j *Job) { j.StartedAt = t0.Add(-time.Minute) }), states: []Job{complete}}, true, "", 1, 0},
		{"started at the same instant as the newest object", &fakeIngest{recent: ours(func(j *Job) { j.StartedAt = t0 }), states: []Job{complete}}, true, "", 1, 0},
		{"another plan", &fakeIngest{recent: ours(func(j *Job) { j.Description = "docs-rag plan=old" }), states: []Job{complete}}, true, "", 1, 0},
		{"a running job is waited for, then the rules apply", &fakeIngest{recent: []Job{{ID: "r", Status: StatusInProgress}}, afterWait: []Job{{ID: "r", Status: StatusComplete}}, states: []Job{complete, {ID: "r", Status: StatusComplete}}}, true, "", 1, 1},
		{"running job fails: wait, then start", &fakeIngest{recent: []Job{{ID: "r", Status: StatusStarting}}, afterWait: []Job{{ID: "r", Status: StatusFailed}}, states: []Job{complete, {ID: "r", Status: StatusFailed}}}, true, "", 1, 1},
		{"list error", &fakeIngest{recentErr: boom}, false, "list ingestion jobs: boom", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Syncer{API: tc.api, Description: plan, Sleep: noSleep}
			_, ran, err := s.RunIfStale(t.Context(), time.Hour, t0)
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Errorf("err = %v, want %q", err, tc.wantErr)
			}
			if ran != tc.ran || tc.api.starts != tc.starts || tc.api.gets != tc.gets {
				t.Errorf("ran %v starts %d gets %d, want %v %d %d", ran, tc.api.starts, tc.api.gets, tc.ran, tc.starts, tc.gets)
			}
			if tc.starts > 0 && tc.api.desc != plan {
				t.Errorf("job started with description %q, want %q", tc.api.desc, plan)
			}
		})
	}
}

func TestPlanHashAndDescription(t *testing.T) {
	a, b := NewFile("docs/a.md", "a", validAttrs()), NewFile("docs/b.md", "b", validAttrs())
	h := PlanHash([]File{a, b})
	if h != PlanHash([]File{b, a}) {
		t.Error("the plan hash depends on the order of the files")
	}
	if h == PlanHash([]File{a}) || h == PlanHash([]File{a, NewFile("docs/b.md", "changed", validAttrs())}) {
		t.Error("the plan hash ignores a deleted or changed file")
	}
	if d := PlanDescription(h); !strings.HasPrefix(d, DescriptionPrefix) || len(d) > maxDescriptionLen {
		t.Errorf("description %q: wrong prefix or longer than %d", d, maxDescriptionLen)
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
