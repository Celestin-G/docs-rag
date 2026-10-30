package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/celestingeniaut/docs-rag/internal/kbpublish"
)

// runPublish executes a fresh `publish-kb args...` command (no flag state is
// shared between runs) with the publisher's environment cleared.
func runPublish(t *testing.T, args ...string) (string, error) {
	t.Helper()
	for _, env := range []string{"DOCS_PATH", "SOURCE_BUCKET", "KB_ID", "DS_ID", "AWS_REGION"} {
		t.Setenv(env, "")
	}
	var out bytes.Buffer
	c := newPublishKBCmd()
	c.SetOut(&out)
	c.SetErr(&out)
	c.SilenceUsage, c.SilenceErrors = true, true
	c.SetArgs(args)
	err := c.ExecuteContext(t.Context())
	return out.String(), err
}

// Every refusal below must happen before the docs are even read: the docs path
// does not exist, so reaching the walk would fail with a different error.
func TestPublishKBRefusesBeforeAnyWork(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-docs")
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"region outside Europe", []string{"--docs-path", missing, "--offline", "--region", "us-east-1"}, "only eu-* regions"},
		{"offline cannot publish", []string{"--docs-path", missing, "--offline", "--dry-run=false"}, "--offline cannot publish"},
		{"bucket required online", []string{"--docs-path", missing}, "source bucket is required"},
		{"publish needs kb and data source", []string{"--docs-path", missing, "--bucket", "b", "--dry-run=false", "--kb-id", "k"}, "--kb-id/--data-source-id"},
		{"publish needs a bucket", []string{"--docs-path", missing, "--dry-run=false", "--kb-id", "k", "--data-source-id", "d"}, "source bucket is required"},
		{"docs path required", []string{"--offline"}, "DOCS_PATH"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runPublish(t, tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// writeDocs creates a docs checkout under a fresh git repository.
func writeDocs(t *testing.T, files map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	files["apps/docs/content/schema.ts"] = `{ id: "stack", releases: [{ version: "v3.0" }] }`
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "."},
		{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "x"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return root
}

// A validation error (here two pages on one path, hence one key) stops the run
// before the AWS configuration is loaded, even online with a bucket set.
func TestPublishKBValidationStopsBeforeAWS(t *testing.T) {
	page := "---\npath: /same\ntitle: T\n---\n\n## S\n\ntext\n"
	root := writeDocs(t, map[string]string{"apps/docs/content/pages/a.mdx": page, "apps/docs/content/pages/b.mdx": page})
	// An unreachable endpoint: if the command went on to S3 it would fail with
	// a network error rather than the validation one.
	t.Setenv("AWS_ENDPOINT_URL", "http://127.0.0.1:1")
	out, err := runPublish(t, "--docs-path", root, "--bucket", "b")
	if err == nil || !strings.Contains(err.Error(), "validation errors") || !strings.Contains(out, "duplicate key") {
		t.Errorf("err = %v, output:\n%s", err, out)
	}
}

// The default excludes (files and the byFingerprint dataset) apply, --exclude and
// --exclude-dataset add to them, and --no-default-excludes restores both kinds.
// Each run gets a fresh command, so no flag leaks from one case to the next.
func TestPublishKBExcludes(t *testing.T) {
	const gen = "apps/docs/content/data/generated/"
	page := func(path string) string { return "---\npath: " + path + "\ntitle: T\n---\n\n## S\n\ntext\n" }
	root := writeDocs(t, map[string]string{
		"apps/docs/content/pages/a.mdx":     page("/a"),
		"apps/docs/content/pages/extra.mdx": page("/extra"),
		gen + "stack.oas.json":              `{"stack.oas": {"k": "v"}}`,
		gen + "api.schemas.json":            `{"api.schemas": {"S": "s"}}`,
		gen + "ledger.cli-flags.json":       `{"ledger.cli-flags": {"F": "f"}}`,
		gen + "numscript.snippets.json":     `{"cookbook.numscript.snippets": {"A": "a"}, "cookbook.numscript.byFingerprint": {"F1": "f", "F2": "g"}}`,
	})
	// KB files: a, extra, cli-flags and snippets = 4; the raw dumps add oas 1,
	// schemas 1 and byFingerprint 2.
	for _, tc := range []struct {
		name  string
		args  []string
		files int
		want  []string
	}{
		{"defaults", nil, 4, []string{
			"Active file excludes (2 files left out):", gen + "stack.oas.json", gen + "api.schemas.json",
			"Active dataset excludes (2 chunks left out):", "cookbook.numscript.byFingerprint"}},
		{"--exclude adds to the defaults", []string{"--exclude", "apps/docs/content/pages/extra.mdx"}, 3, []string{
			"Active file excludes (3 files left out):", gen + "stack.oas.json", "apps/docs/content/pages/extra.mdx", "cookbook.numscript.byFingerprint"}},
		{"--exclude-dataset adds to the defaults", []string{"--exclude-dataset", "cookbook.numscript.snippets"}, 3, []string{
			"Active dataset excludes (3 chunks left out):", "cookbook.numscript.byFingerprint", "cookbook.numscript.snippets", gen + "api.schemas.json"}},
		{"--no-default-excludes", []string{"--no-default-excludes"}, 8, []string{"Active file excludes: none", "Active dataset excludes: none"}},
		{"--no-default-excludes with --exclude", []string{"--no-default-excludes", "--exclude", "apps/docs/content/pages/extra.mdx"}, 7, []string{
			"Active file excludes (1 files left out):", "Active dataset excludes: none"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runPublish(t, append([]string{"--docs-path", root, "--offline"}, tc.args...)...)
			if err != nil {
				t.Fatalf("err = %v\n%s", err, out)
			}
			if m := regexp.MustCompile(`Validation passed: (\d+) files`).FindStringSubmatch(out); m == nil || m[1] != fmt.Sprint(tc.files) {
				t.Errorf("want %d files, output:\n%s", tc.files, out)
			}
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("output lacks %q:\n%s", w, out)
				}
			}
		})
	}
}

// --- crash-resume --------------------------------------------------------

type fakeDates struct{}

func (fakeDates) Date(string) (string, bool) { return "2026-10-30", true }

// memBucket is an in-memory bucket whose writes are stamped with now.
type memBucket struct {
	mu   sync.Mutex
	now  time.Time
	objs map[string]kbpublish.Object
	hash map[string]string
}

func newBucket() *memBucket {
	return &memBucket{objs: map[string]kbpublish.Object{}, hash: map[string]string{}}
}

func (b *memBucket) List(_ context.Context, prefix string) ([]kbpublish.Object, error) {
	var out []kbpublish.Object
	for k, o := range b.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, o)
		}
	}
	return out, nil
}
func (b *memBucket) Hash(_ context.Context, key string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.hash[key], nil
}
func (b *memBucket) Put(_ context.Context, key string, _ []byte, _, hash string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.objs[key], b.hash[key] = kbpublish.Object{Key: key, Modified: b.now}, hash
	return nil
}
func (b *memBucket) Delete(_ context.Context, keys []string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, k := range keys {
		delete(b.objs, k)
		delete(b.hash, k)
	}
	return nil
}

// seed writes every file of the plan at the given time, as an earlier run did.
func (b *memBucket) seed(plan *kbpublish.Plan, at time.Time) {
	b.now = at
	for _, f := range plan.Files {
		_ = b.Put(context.Background(), kbpublish.SidecarKey(f.Key), f.Sidecar, "", f.Hash)
		_ = b.Put(context.Background(), f.Key, f.Body, "", f.Hash)
	}
}

// fakeAPI is a data source with a job history, newest first. A started job is
// COMPLETE at once; a running job completes on the first Get.
type fakeAPI struct {
	jobs     []kbpublish.Job
	startErr error // returned once by the next Start
	now      time.Time
	starts   []string // descriptions of the jobs started
	gets     int
	final    kbpublish.Job // when set, the job every Start returns
}

func (f *fakeAPI) Start(_ context.Context, desc string) (kbpublish.Job, error) {
	if err := f.startErr; err != nil {
		f.startErr = nil
		return kbpublish.Job{}, err
	}
	f.starts = append(f.starts, desc)
	j := kbpublish.Job{ID: fmt.Sprintf("n%d", len(f.starts)), Description: desc, StartedAt: f.now, Status: kbpublish.StatusComplete}
	if f.final.ID != "" {
		j = f.final
	}
	f.jobs = append([]kbpublish.Job{j}, f.jobs...)
	return j, nil
}

func (f *fakeAPI) Get(_ context.Context, id string) (kbpublish.Job, error) {
	f.gets++
	for i := range f.jobs {
		if f.jobs[i].ID == id {
			f.jobs[i].Status = kbpublish.StatusComplete
			return f.jobs[i], nil
		}
	}
	return kbpublish.Job{}, errors.New("no such job " + id)
}

func (f *fakeAPI) Recent(context.Context, int) ([]kbpublish.Job, error) {
	return append([]kbpublish.Job(nil), f.jobs...), nil
}

func testPlan(t *testing.T) (*kbpublish.Plan, string) {
	t.Helper()
	table, err := kbpublish.ParseSchema(`{ id: "stack", releases: [{ version: "v3.0" }] }`)
	if err != nil {
		t.Fatal(err)
	}
	page := func(p string) []byte { return []byte("---\npath: " + p + "\ntitle: T\n---\n\n## S\n\ntext\n") }
	plan := kbpublish.Build([]kbpublish.SourceFile{
		{Rel: "apps/docs/content/pages/a.mdx", Content: page("/a")},
		{Rel: "apps/docs/content/pages/b.mdx", Content: page("/b")},
	}, kbpublish.Options{Table: table, Dates: fakeDates{}})
	if len(plan.Errors) != 0 || len(plan.Files) != 2 {
		t.Fatalf("plan: %v %d files", plan.Errors, len(plan.Files))
	}
	return plan, kbpublish.PlanDescription(kbpublish.PlanHash(plan.Files))
}

// Regression: after the S3 writes succeed, a crash or a failed StartIngestionJob
// leaves the knowledge base stale; the next run sees an empty diff and must not
// trust an older COMPLETE job.
func TestPublishResumesAfterCrash(t *testing.T) {
	plan, planDesc := testPlan(t)
	t0 := time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)
	t1, t2 := t0.Add(time.Hour), t0.Add(2*time.Hour)
	const oldPlan = "docs-rag plan=0000"
	job := func(id, desc, status string, at time.Time) kbpublish.Job {
		return kbpublish.Job{ID: id, Description: desc, Status: status, StartedAt: at}
	}

	for _, tc := range []struct {
		name       string
		objectsAt  time.Time
		jobs       []kbpublish.Job
		wantStarts int
		wantGets   int
		wantOut    string
	}{
		{"(b) uploads done, crash before Start: the COMPLETE job is older than the objects",
			t1, []kbpublish.Job{job("j", planDesc, kbpublish.StatusComplete, t0)}, 1, 0, "Ingestion job n1"},
		{"(c) delete-only crash: job newer than every object but for another plan",
			t0, []kbpublish.Job{job("j", oldPlan, kbpublish.StatusComplete, t1)}, 1, 0, "Ingestion job n1"},
		{"(d) everything consistent: no sync",
			t0, []kbpublish.Job{job("j", planDesc, kbpublish.StatusComplete, t1), job("older", oldPlan, kbpublish.StatusComplete, t0)}, 0, 0, "no new job"},
		{"(d) a manual job on top does not hide ours",
			t0, []kbpublish.Job{job("m", "manual", kbpublish.StatusComplete, t2), job("j", planDesc, kbpublish.StatusComplete, t1)}, 0, 0, "no new job"},
		{"(e) a running job of ours is waited for, then re-evaluated: consistent",
			t0, []kbpublish.Job{job("r", planDesc, kbpublish.StatusInProgress, t1)}, 0, 1, "no new job"},
		{"(e) a running job of another plan is waited for, then a sync starts",
			t0, []kbpublish.Job{job("r", oldPlan, kbpublish.StatusInProgress, t1)}, 1, 1, "Ingestion job n1"},
		{"the last job completed with failed documents",
			t0, []kbpublish.Job{{ID: "j", Description: planDesc, Status: kbpublish.StatusComplete, StartedAt: t1, Stats: kbpublish.JobStats{Failed: 1}}}, 1, 0, "Ingestion job n1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bucket, api := newBucket(), &fakeAPI{jobs: tc.jobs, now: t2}
			bucket.seed(plan, tc.objectsAt)
			var out bytes.Buffer
			err := publish(t.Context(), &out, bucket, api, plan, false, false, time.Millisecond, time.Minute, false)
			if err != nil {
				t.Fatalf("err = %v\n%s", err, out.String())
			}
			if len(api.starts) != tc.wantStarts || api.gets != tc.wantGets || !strings.Contains(out.String(), tc.wantOut) {
				t.Errorf("starts %v gets %d, want %d %d; output:\n%s", api.starts, api.gets, tc.wantStarts, tc.wantGets, out.String())
			}
			for _, d := range api.starts {
				if d != planDesc {
					t.Errorf("job started with description %q, want %q", d, planDesc)
				}
			}
		})
	}

	t.Run("(a) uploads succeed, Start fails: the retry starts a sync", func(t *testing.T) {
		bucket := newBucket()
		bucket.now = t1
		api := &fakeAPI{jobs: []kbpublish.Job{job("old", oldPlan, kbpublish.StatusComplete, t0)}, now: t2, startErr: errors.New("throttled")}
		var out bytes.Buffer
		err := publish(t.Context(), &out, bucket, api, plan, false, false, time.Millisecond, time.Minute, false)
		if err == nil || !strings.Contains(err.Error(), "throttled") || !strings.Contains(err.Error(), "--force-sync") || len(bucket.objs) != 4 {
			t.Fatalf("first run: err = %v, %d objects", err, len(bucket.objs))
		}
		out.Reset()
		bucket.now = t2 // not written to again: the diff is empty
		if err := publish(t.Context(), &out, bucket, api, plan, false, false, time.Millisecond, time.Minute, false); err != nil {
			t.Fatalf("retry: %v\n%s", err, out.String())
		}
		if len(api.starts) != 1 || api.starts[0] != planDesc || !strings.Contains(out.String(), "Uploaded 0 files, deleted 0 objects") {
			t.Errorf("retry: starts %v, output:\n%s", api.starts, out.String())
		}
	})
}

func TestSyncAfterPublish(t *testing.T) {
	failedJob := kbpublish.Job{ID: "j2", Status: kbpublish.StatusFailed, FailureReasons: []string{"no access"}}
	now := time.Now()
	recent := []kbpublish.Job{{ID: "j1", Description: "docs-rag plan=x", Status: kbpublish.StatusComplete, StartedAt: now.Add(time.Hour)}}
	for _, tc := range []struct {
		name           string
		api            *fakeAPI
		changed, force bool
		wantErr        bool
		wantOut        string
	}{
		{"changed: the jobs are not consulted", &fakeAPI{jobs: recent, now: now}, true, false, false, "COMPLETE"},
		{"forced", &fakeAPI{jobs: recent, now: now}, false, true, false, "COMPLETE"},
		{"failure carries the hint", &fakeAPI{final: failedJob}, true, false, true, "failure: no access"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := syncAfterPublish(t.Context(), &out, tc.api, "docs-rag plan=x", now, tc.changed, tc.force, time.Millisecond, time.Minute)
			if (err != nil) != tc.wantErr || len(tc.api.starts) != 1 || !strings.Contains(out.String(), tc.wantOut) {
				t.Errorf("err = %v starts = %v out = %q", err, tc.api.starts, out.String())
			}
			if tc.wantErr && !strings.Contains(err.Error(), "re-run with") {
				t.Errorf("error lacks the re-run hint: %v", err)
			}
		})
	}
}
