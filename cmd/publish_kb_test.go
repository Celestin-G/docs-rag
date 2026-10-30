package cmd

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/celestingeniaut/docs-rag/internal/kbpublish"
	"github.com/spf13/pflag"
)

// runPublish executes `rag publish-kb args...` with every flag back at its
// default and the publisher's environment cleared.
func runPublish(t *testing.T, args ...string) (string, error) {
	t.Helper()
	for _, env := range []string{"DOCS_PATH", "SOURCE_BUCKET", "KB_ID", "DS_ID", "AWS_REGION"} {
		t.Setenv(env, "")
	}
	publishKBCmd.Flags().VisitAll(func(f *pflag.Flag) {
		if f.Value.Type() != "stringArray" {
			_ = f.Value.Set(f.DefValue)
		}
		f.Changed = false
	})
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SilenceUsage, rootCmd.SilenceErrors = true, true
	t.Cleanup(func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
		rootCmd.SilenceUsage, rootCmd.SilenceErrors = false, false
	})
	// Cobra hands the root context to a subcommand only once: reset it, or a
	// later test runs with an earlier test's cancelled context.
	publishKBCmd.SetContext(t.Context())
	rootCmd.SetArgs(append([]string{"publish-kb"}, args...))
	err := rootCmd.ExecuteContext(t.Context())
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

// The default build cannot start an ingestion job, so --dry-run=false must stop
// before any S3 write: here, before the docs are even read.
func TestPublishKBWithoutBedrockRefusesToWrite(t *testing.T) {
	if kbpublish.IngestSupported {
		t.Skip("built with -tags bedrockagent")
	}
	missing := filepath.Join(t.TempDir(), "no-such-docs")
	_, err := runPublish(t, "--docs-path", missing, "--bucket", "b", "--kb-id", "k", "--data-source-id", "d", "--dry-run=false")
	if err == nil || !strings.Contains(err.Error(), "bedrockagent") {
		t.Errorf("err = %v, want the bedrockagent refusal", err)
	}
}

// A validation error (here two pages on one path, hence one key) stops the run
// before the AWS configuration is loaded, even online with a bucket set.
func TestPublishKBValidationStopsBeforeAWS(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("apps/docs/content/schema.ts", `{ id: "stack", releases: [{ version: "v3.0" }] }`)
	page := "---\npath: /same\ntitle: T\n---\n\n## S\n\ntext\n"
	write("apps/docs/content/pages/a.mdx", page)
	write("apps/docs/content/pages/b.mdx", page)
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "."},
		{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "x"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	// An unreachable endpoint: if the command went on to S3 it would fail with
	// a network error rather than the validation one.
	t.Setenv("AWS_ENDPOINT_URL", "http://127.0.0.1:1")
	out, err := runPublish(t, "--docs-path", root, "--bucket", "b")
	if err == nil || !strings.Contains(err.Error(), "validation errors") || !strings.Contains(out, "duplicate key") {
		t.Errorf("err = %v, output:\n%s", err, out)
	}
}

type fakeAPI struct {
	latest   kbpublish.Job
	latestOK bool
	final    kbpublish.Job
	starts   int
}

func (f *fakeAPI) Start(context.Context) (kbpublish.Job, error) { f.starts++; return f.final, nil }
func (f *fakeAPI) Get(context.Context, string) (kbpublish.Job, error) {
	return f.final, nil
}
func (f *fakeAPI) Latest(context.Context) (kbpublish.Job, bool, error) {
	return f.latest, f.latestOK, nil
}

func TestSyncAfterPublish(t *testing.T) {
	complete := func(id string) kbpublish.Job { return kbpublish.Job{ID: id, Status: kbpublish.StatusComplete} }
	failedJob := kbpublish.Job{ID: "j2", Status: kbpublish.StatusFailed, FailureReasons: []string{"no access"}}
	for _, tc := range []struct {
		name           string
		api            *fakeAPI
		changed, force bool
		starts         int
		wantErr        bool
		wantOut        string
	}{
		{"nothing changed, last job fine", &fakeAPI{latest: complete("j1"), latestOK: true}, false, false, 0, false, "no new job"},
		{"nothing changed, last job failed", &fakeAPI{latest: kbpublish.Job{ID: "j1", Status: kbpublish.StatusFailed}, latestOK: true, final: complete("j2")}, false, false, 1, false, "Ingestion job j2: COMPLETE"},
		{"changed: the last job is not consulted", &fakeAPI{latest: complete("j1"), latestOK: true, final: complete("j2")}, true, false, 1, false, "COMPLETE"},
		{"forced", &fakeAPI{latest: complete("j1"), latestOK: true, final: complete("j2")}, false, true, 1, false, "COMPLETE"},
		{"failure carries the hint", &fakeAPI{final: failedJob}, true, false, 1, true, "failure: no access"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := syncAfterPublish(t.Context(), &out, tc.api, tc.changed, tc.force, time.Millisecond, time.Minute)
			if (err != nil) != tc.wantErr || tc.api.starts != tc.starts || !strings.Contains(out.String(), tc.wantOut) {
				t.Errorf("err = %v starts = %d out = %q", err, tc.api.starts, out.String())
			}
			if tc.wantErr && !strings.Contains(err.Error(), "re-run with") {
				t.Errorf("error lacks the re-run hint: %v", err)
			}
		})
	}
}
