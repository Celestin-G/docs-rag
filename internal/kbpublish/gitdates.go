package kbpublish

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
)

// DateSource gives the updated_at (YYYY-MM-DD) of a docs file.
type DateSource interface {
	// Date returns the date for a path relative to the docs root and whether it
	// came from the file's own history (false: the fallback date was used).
	Date(rel string) (date string, own bool)
}

// GitDates answers from one pass over `git log`: reading the history once is
// cheap, one process per file would be 300 of them.
type GitDates struct {
	byFile   map[string]string
	fallback string
}

// Date implements DateSource.
func (g *GitDates) Date(rel string) (string, bool) {
	if d, ok := g.byFile[rel]; ok {
		return d, true
	}
	return g.fallback, false
}

var dateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// LoadGitDates runs git once over the repository at root. Files absent from the
// log (untracked, or outside the pathspec) get the date of the HEAD commit.
func LoadGitDates(ctx context.Context, root string) (*GitDates, error) {
	head, err := gitOutput(ctx, root, "log", "-1", "--date=short", "--format=%cd")
	if err != nil {
		return nil, fmt.Errorf("read the HEAD commit date of %s: %w", root, err)
	}
	fallback := strings.TrimSpace(head)
	if !dateRe.MatchString(fallback) {
		return nil, fmt.Errorf("git log in %s: unexpected HEAD date %q", root, fallback)
	}

	cmd := exec.CommandContext(ctx, "git", "-C", root, "-c", "core.quotepath=false",
		"log", "--name-only", "--no-renames", "--date=short", "--format=%x01%cd", "--relative", "--", ".")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("git log in %s: %w", root, err)
	}
	by, parseErr := parseGitLog(stdout)
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("git log in %s: %w", root, err)
	}
	if parseErr != nil {
		return nil, parseErr
	}
	return &GitDates{byFile: by, fallback: fallback}, nil
}

func gitOutput(ctx context.Context, root string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...).Output()
	return string(out), err
}

// parseGitLog reads `git log --name-only --format=%x01%cd` output, newest commit
// first, and keeps for each file the date of the first (latest) commit that
// touched it.
func parseGitLog(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var date string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "\x01"):
			date = line[1:]
			if !dateRe.MatchString(date) {
				return nil, fmt.Errorf("git log: unexpected date %q", date)
			}
		case line == "":
		case date != "":
			if _, seen := out[line]; !seen {
				out[line] = date
			}
		}
	}
	return out, sc.Err()
}
