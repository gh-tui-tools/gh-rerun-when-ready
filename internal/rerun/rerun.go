// Package rerun decides which of a pull request's workflow runs are still
// going, and which of the finished ones have failed jobs worth re-running.
package rerun

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Run is the part of a workflow run this tool cares about.
type Run struct {
	DatabaseID   int64  `json:"id"`
	WorkflowName string `json:"name"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
	Event        string `json:"event"`
	URL          string `json:"html_url"`
}

// A run in any of these states still has work to do, and GitHub refuses to
// re-run the failed jobs of a run that is still going.
var pendingStatus = map[string]bool{
	"queued":      true,
	"in_progress": true,
	"waiting":     true,
	"requested":   true,
	"pending":     true,
}

// Conclusions whose failed jobs can be re-run.
var rerunnableConclusion = map[string]bool{
	"failure":   true,
	"timed_out": true,
	"cancelled": true,
}

// Conclusions that are finished and need nothing done.
var settledConclusion = map[string]bool{
	"success": true,
	"skipped": true,
	"neutral": true,
}

// IsPending reports whether the run is still going.
func IsPending(r Run) bool { return pendingStatus[r.Status] }

// IsRerunnable reports whether the run finished in a state whose failed jobs
// GitHub will re-run.
func IsRerunnable(r Run) bool {
	return r.Status == "completed" && rerunnableConclusion[r.Conclusion]
}

// NeedsAttention reports whether the run finished in a state that this tool
// cannot act on, such as waiting for a maintainer to approve it.
func NeedsAttention(r Run) bool {
	return r.Status == "completed" &&
		!rerunnableConclusion[r.Conclusion] &&
		!settledConclusion[r.Conclusion]
}

// Buckets sorts a pull request's runs by what should happen to each.
type Buckets struct {
	// Pending runs are still going, so their own failed jobs can't be
	// re-run yet. They do not hold back any other run.
	Pending []Run
	// Rerun are finished, failed, and have attempts left.
	Rerun []Run
	// Exhausted are finished and failed, with no attempts left.
	Exhausted []Run
	// Attention are finished in a state this tool cannot act on.
	Attention []Run
}

// Classify sorts runs into buckets. attempts records how many times each run
// has already been re-run, and maxAttempts caps that.
func Classify(runs []Run, attempts map[int64]int, maxAttempts int) Buckets {
	var b Buckets
	for _, r := range runs {
		switch {
		case IsPending(r):
			b.Pending = append(b.Pending, r)
		case IsRerunnable(r):
			if attempts[r.DatabaseID] < maxAttempts {
				b.Rerun = append(b.Rerun, r)
			} else {
				b.Exhausted = append(b.Exhausted, r)
			}
		case NeedsAttention(r):
			b.Attention = append(b.Attention, r)
		}
	}
	return b
}

// Describe names a run for a log line.
func Describe(r Run) string {
	return fmt.Sprintf("%s (%d)", r.WorkflowName, r.DatabaseID)
}

var prURL = regexp.MustCompile(`^https?://[^/]+/([^/]+/[^/]+)/pull/(\d+)`)

// ParseRef turns a pull request number or URL, plus the value of any --repo
// flag, into a repository and a number. An empty repository means the caller
// should fall back to the repository of the working directory.
func ParseRef(target, repoFlag string) (repo string, number int, err error) {
	if m := prURL.FindStringSubmatch(target); m != nil {
		if repoFlag != "" && repoFlag != m[1] {
			return "", 0, fmt.Errorf("--repo %s conflicts with the repository in the URL", repoFlag)
		}
		n, _ := strconv.Atoi(m[2])
		return m[1], n, nil
	}
	n, convErr := strconv.Atoi(strings.TrimPrefix(target, "#"))
	if convErr != nil || n <= 0 {
		return "", 0, fmt.Errorf("not a pull request number or URL: %s", target)
	}
	return repoFlag, n, nil
}
