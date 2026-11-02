package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/celestingeniaut/docs-rag/internal/docsource"
	"github.com/celestingeniaut/docs-rag/internal/kbpublish"
	"github.com/spf13/cobra"
)

// newPublishKBCmd builds the publish-kb command. init registers one; each test
// builds its own so no flag state is shared between runs.
func newPublishKBCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "publish-kb",
		Short: "Publish the documentation to an Amazon Bedrock Knowledge Base (S3 files, metadata sidecars, ingestion job)",
		Long: `Turns the docs into one markdown file plus one metadata sidecar per chunk, diffs
them against s3://<bucket>/docs/, uploads what is new or changed, deletes what is
stale and starts one ingestion job.

It is a dry run unless --dry-run=false is passed. --offline makes no AWS call at
all: it diffs against an empty bucket.`,
		RunE: runPublishKB,
	}
	f := c.Flags()
	f.Bool("dry-run", true, "Report what would change and write nothing (use --dry-run=false to publish)")
	f.Bool("offline", false, "Dry run without any AWS call: diff against an empty bucket")
	f.String("out", "", "Also write the generated files and sidecars under this directory")
	f.String("docs-path", "", "Docs checkout (default: $DOCS_PATH)")
	f.String("bucket", "", "Source bucket of the knowledge base (default: $SOURCE_BUCKET)")
	f.String("kb-id", "", "Knowledge base id (default: $KB_ID)")
	f.String("data-source-id", "", "Data source id (default: $DS_ID)")
	f.String("region", "", "AWS region, eu-* only (default: $AWS_REGION, else eu-west-1)")
	f.Bool("allow-mass-delete", false, "Allow deleting more than 20% of the existing objects")
	f.Bool("force-sync", false, "Start an ingestion job even when nothing changed")
	f.Duration("timeout", 30*time.Minute, "How long to wait for the ingestion job")
	f.StringArray("exclude", nil, "Glob on repo-relative paths to leave out, added to the default excludes (repeatable). Excluding a file that is already published deletes it from the bucket (subject to the 20% delete guard)")
	f.StringArray("exclude-dataset", nil, "Glob on the name of a dataset inside the generated JSON files to leave out, added to the default dataset excludes (repeatable)")
	f.Bool("no-default-excludes", false, "Publish what is excluded by default too: files "+strings.Join(kbpublish.DefaultExcludes, ", ")+"; datasets "+strings.Join(kbpublish.DefaultDatasetExcludes, ", "))
	f.Bool("verbose", false, "List every key of the diff")
	return c
}

func init() {
	rootCmd.AddCommand(newPublishKBCmd())
}

// resolveExcludes returns the file and dataset excludes in force: the defaults
// (unless --no-default-excludes) followed by the repeatable flags.
func resolveExcludes(cmd *cobra.Command) (files, datasets []string) {
	fl := cmd.Flags()
	files, _ = fl.GetStringArray("exclude")
	datasets, _ = fl.GetStringArray("exclude-dataset")
	if noDefault, _ := fl.GetBool("no-default-excludes"); !noDefault {
		files = append(slices.Clone(kbpublish.DefaultExcludes), files...)
		datasets = append(slices.Clone(kbpublish.DefaultDatasetExcludes), datasets...)
	}
	return files, datasets
}

// envDefault returns the flag value, else the environment variable, else def.
func envDefault(cmd *cobra.Command, flag, env, def string) string {
	if v, _ := cmd.Flags().GetString(flag); v != "" {
		return v
	}
	if v := os.Getenv(env); v != "" {
		return v
	}
	return def
}

func runPublishKB(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	fl := cmd.Flags()
	dry, _ := fl.GetBool("dry-run")
	offline, _ := fl.GetBool("offline")
	out, _ := fl.GetString("out")
	allowMass, _ := fl.GetBool("allow-mass-delete")
	force, _ := fl.GetBool("force-sync")
	timeout, _ := fl.GetDuration("timeout")
	excludes, datasetExcludes := resolveExcludes(cmd)
	verbose, _ := fl.GetBool("verbose")

	docsPath := envDefault(cmd, "docs-path", "DOCS_PATH", "")
	bucket := envDefault(cmd, "bucket", "SOURCE_BUCKET", "")
	kbID := envDefault(cmd, "kb-id", "KB_ID", "")
	dsID := envDefault(cmd, "data-source-id", "DS_ID", "")
	region := envDefault(cmd, "region", "AWS_REGION", kbpublish.DefaultRegion)

	if docsPath == "" {
		return errors.New("DOCS_PATH (or --docs-path) is required")
	}
	if offline && !fl.Changed("dry-run") {
		dry = true
	}
	if offline && !dry {
		return errors.New("--offline cannot publish: it makes no AWS call")
	}
	if err := kbpublish.CheckRegion(region); err != nil {
		return err
	}
	if !offline {
		if bucket == "" {
			return errors.New("the source bucket is required: --bucket or SOURCE_BUCKET")
		}
		if !dry && (kbID == "" || dsID == "") {
			return errors.New("--kb-id/--data-source-id (KB_ID, DS_ID) are required to publish")
		}
	}

	// Everything below up to the AWS calls is local and read-only.
	schema, err := os.ReadFile(filepath.Join(docsPath, "apps", "docs", "content", "schema.ts"))
	if err != nil {
		return fmt.Errorf("read the Stack release table: %w", err)
	}
	table, err := kbpublish.ParseSchema(string(schema))
	if err != nil {
		return err
	}
	paths, err := docsource.Walk(docsPath)
	if err != nil {
		return fmt.Errorf("walk %s: %w", docsPath, err)
	}
	sources := make([]kbpublish.SourceFile, 0, len(paths))
	for _, p := range paths {
		rel, _ := filepath.Rel(docsPath, p)
		content, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("read %s: %w", rel, err)
		}
		sources = append(sources, kbpublish.SourceFile{Rel: filepath.ToSlash(rel), Content: content})
	}
	dates, err := kbpublish.LoadGitDates(ctx, docsPath)
	if err != nil {
		return err
	}

	plan := kbpublish.Build(sources, kbpublish.Options{Table: table, Dates: dates, Excludes: excludes, DatasetExcludes: datasetExcludes})
	stdout := cmd.OutOrStdout()
	plan.Report.PrintSummary(stdout, table)
	printExcludes(stdout, plan.Report, excludes, datasetExcludes)

	if len(plan.Report.ParseFailures) > 0 {
		return fmt.Errorf("%d files failed to parse: nothing published", len(plan.Report.ParseFailures))
	}
	if len(plan.Errors) > 0 {
		fmt.Fprintf(stdout, "\nVALIDATION FAILED: %d errors, no AWS call made\n", len(plan.Errors))
		for _, e := range plan.Errors {
			fmt.Fprintf(stdout, "  %s\n", e)
		}
		return fmt.Errorf("%d validation errors", len(plan.Errors))
	}
	if len(plan.Files) == 0 {
		return errors.New("no file produced: a publish would empty the knowledge base")
	}
	fmt.Fprintf(stdout, "\nValidation passed: %d files.\n", len(plan.Files))

	if out != "" {
		if err := writeOut(out, plan.Files); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Wrote %d files and sidecars under %s\n", len(plan.Files), out)
	}

	var store kbpublish.ObjectStore = kbpublish.EmptyStore{}
	if !offline {
		cfg, err := kbpublish.LoadAWSConfig(ctx, region)
		if err != nil {
			return fmt.Errorf("load AWS config: %w", err)
		}
		store = kbpublish.NewS3Store(cfg, bucket)
		if !dry {
			api := kbpublish.NewIngestAPI(cfg, kbID, dsID)
			return publish(ctx, stdout, store, api, plan, allowMass, force, 10*time.Second, timeout, verbose)
		}
	}

	diff, err := kbpublish.ComputeDiff(ctx, store, plan.Files, 0)
	if err != nil {
		return err
	}
	diff.PrintDiff(stdout, verbose)
	if err := diff.CheckDeletes(allowMass); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "\nDry run: nothing written to the bucket.")
	return nil
}

func writeOut(dir string, files []kbpublish.File) error {
	for _, f := range files {
		p := filepath.Join(dir, filepath.FromSlash(f.Key))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, f.Body, 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(kbpublish.SidecarKey(p), f.Sidecar, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// printExcludes lists the file and dataset excludes in force and what they left out.
func printExcludes(w io.Writer, rep kbpublish.Report, files, datasets []string) {
	if len(files) == 0 {
		fmt.Fprintln(w, "\nActive file excludes: none")
	} else {
		fmt.Fprintf(w, "\nActive file excludes (%d files left out):\n", rep.Excluded)
		for _, e := range files {
			fmt.Fprintf(w, "  %s\n", e)
		}
	}
	if len(datasets) == 0 {
		fmt.Fprintln(w, "Active dataset excludes: none")
		return
	}
	left := 0
	for _, n := range rep.DatasetExcluded {
		left += n
	}
	fmt.Fprintf(w, "Active dataset excludes (%d chunks left out):\n", left)
	for _, e := range datasets {
		fmt.Fprintf(w, "  %s\n", e)
	}
}

// publish runs the real thing: diff, upload, delete, then one ingestion job.
func publish(ctx context.Context, w io.Writer, store kbpublish.ObjectStore, api kbpublish.IngestAPI, plan *kbpublish.Plan,
	allowMass, forceSync bool, poll, timeout time.Duration, verbose bool) error {
	diff, err := kbpublish.ComputeDiff(ctx, store, plan.Files, 0)
	if err != nil {
		return err
	}
	diff.PrintDiff(w, verbose)
	if err := kbpublish.Apply(ctx, store, diff, plan.Files, allowMass, 0); err != nil {
		return err
	}
	fmt.Fprintf(w, "\nUploaded %d files, deleted %d objects.\n", len(diff.New)+len(diff.Changed), len(diff.DeleteKeys()))

	desc := kbpublish.PlanDescription(kbpublish.PlanHash(plan.Files))
	return syncAfterPublish(ctx, w, api, desc, diff.Newest, diff.Changes(), forceSync, poll, timeout)
}

// syncHint is added to every ingestion failure.
const syncHint = "re-run with --force-sync to start a new ingestion job"

// syncAfterPublish starts the ingestion job a publish needs. When the bucket
// changed (or a sync is forced) it always starts one. Otherwise the knowledge
// base can still be stale (an earlier run crashed after its S3 writes, or its job
// failed), so the latest docs-rag job decides, see Syncer.RunIfStale. Every job
// carries desc, "docs-rag plan=<hash>". newest is the latest LastModified under
// docs/ before this run.
func syncAfterPublish(ctx context.Context, w io.Writer, api kbpublish.IngestAPI, desc string, newest time.Time, changed, forced bool, poll, timeout time.Duration) error {
	syncer := &kbpublish.Syncer{API: api, Description: desc, PollInterval: poll, Log: os.Stderr}
	var (
		job kbpublish.Job
		ran = true
		err error
	)
	if changed || forced {
		job, err = syncer.Run(ctx, timeout)
	} else {
		job, ran, err = syncer.RunIfStale(ctx, timeout, newest)
	}
	if err == nil && !ran {
		fmt.Fprintf(w, "Nothing changed and the latest docs-rag ingestion job %s already ingested this plan: no new job (use --force-sync to start one anyway).\n", job.ID)
		return nil
	}
	if job.ID != "" {
		fmt.Fprint(w, job.Summary())
	}
	if err != nil {
		return fmt.Errorf("%w (%s)", err, syncHint)
	}
	return nil
}
