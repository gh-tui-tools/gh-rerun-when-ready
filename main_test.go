package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gh-tui-tools/gh-rerun-when-ready/internal/rerun"
)

func TestParseArgs(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		o, err := parseArgs([]string{"123"})
		if err != nil || o.target != "123" || o.interval != 60*time.Second ||
			o.attempts != 1 || o.dryRun || o.timeout != 0 {
			t.Fatalf("unexpected: %+v err=%v", o, err)
		}
	})
	t.Run("long and short forms agree", func(t *testing.T) {
		a, _ := parseArgs([]string{"-R", "o/r", "-i", "5", "-a", "2", "-t", "9", "-n", "1"})
		b, _ := parseArgs([]string{"--repo", "o/r", "--interval", "5",
			"--attempts", "2", "--timeout", "9", "--dry-run", "1"})
		if a != b {
			t.Fatalf("short %+v differs from long %+v", a, b)
		}
		if a.repo != "o/r" || a.interval != 5*time.Second || a.attempts != 2 ||
			a.timeout != 9*time.Second || !a.dryRun {
			t.Fatalf("unexpected: %+v", a)
		}
	})
	t.Run("help needs no pull request", func(t *testing.T) {
		o, err := parseArgs([]string{"--help"})
		if err != nil || !o.help {
			t.Fatalf("unexpected: %+v err=%v", o, err)
		}
	})
	t.Run("a pull request is required", func(t *testing.T) {
		if _, err := parseArgs(nil); err == nil {
			t.Fatal("want an error")
		}
	})
	t.Run("unknown option is an error", func(t *testing.T) {
		if _, err := parseArgs([]string{"--nope", "1"}); err == nil {
			t.Fatal("want an error, so that a typo is not read as the pull request")
		}
	})
	t.Run("two pull requests is an error", func(t *testing.T) {
		if _, err := parseArgs([]string{"1", "2"}); err == nil {
			t.Fatal("want an error")
		}
	})
	t.Run("bad values are errors", func(t *testing.T) {
		for _, argv := range [][]string{
			{"-i", "0", "1"}, {"-i", "x", "1"}, {"-a", "0", "1"},
			{"-t", "-1", "1"}, {"--repo"}, {"-i"},
		} {
			if _, err := parseArgs(argv); err == nil {
				t.Fatalf("want an error for %v", argv)
			}
		}
	})
}

// fakeClient serves a scripted sequence of run lists and records re-runs.
type fakeClient struct {
	sha      string
	polls    [][]rerun.Run
	call     int
	reruns   []int64
	failNext bool
}

func (f *fakeClient) pullRequest(string, int) (string, string, string, error) {
	return f.sha, "open", "A title", nil
}

func (f *fakeClient) runsForCommit(string, string) ([]rerun.Run, error) {
	i := f.call
	if i >= len(f.polls) {
		i = len(f.polls) - 1
	}
	f.call++
	return f.polls[i], nil
}

func (f *fakeClient) rerunFailedJobs(_ string, id int64) error {
	f.reruns = append(f.reruns, id)
	if f.failNext {
		f.failNext = false
		return os.ErrPermission
	}
	return nil
}

func fast(o options) options {
	o.interval = time.Millisecond
	if o.attempts == 0 {
		o.attempts = 1
	}
	return o
}

func TestWatchWaitsForPendingRunsBeforeRerunning(t *testing.T) {
	failed := rerun.Run{DatabaseID: 7, WorkflowName: "CI",
		Status: "completed", Conclusion: "failure"}
	pending := rerun.Run{DatabaseID: 8, WorkflowName: "Lint",
		Status: "in_progress"}
	done := rerun.Run{DatabaseID: 8, WorkflowName: "Lint",
		Status: "completed", Conclusion: "success"}
	reran := rerun.Run{DatabaseID: 7, WorkflowName: "CI",
		Status: "completed", Conclusion: "success"}

	f := &fakeClient{sha: "abc1234567", polls: [][]rerun.Run{
		{failed, pending}, // Lint still going: must not re-run yet
		{failed, pending}, // still going
		{failed, done},    // now everything has finished: re-run CI
		{reran, done},     // the re-run succeeded
	}}

	var out bytes.Buffer
	code := watch(f, fast(options{repo: "o/r", number: 1}), &out, nil)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out.String())
	}
	if len(f.reruns) != 1 || f.reruns[0] != 7 {
		t.Fatalf("re-runs = %v, want exactly [7]\n%s", f.reruns, out.String())
	}
	log := out.String()
	if !strings.Contains(log, "waiting on 1 run(s)") {
		t.Errorf("expected it to report waiting:\n%s", log)
	}
	if !strings.Contains(log, "every run for this commit succeeded") {
		t.Errorf("expected a success line:\n%s", log)
	}
}

func TestWatchDryRunRerunsNothing(t *testing.T) {
	failed := rerun.Run{DatabaseID: 7, WorkflowName: "CI",
		Status: "completed", Conclusion: "failure"}
	f := &fakeClient{sha: "abc", polls: [][]rerun.Run{{failed}}}

	var out bytes.Buffer
	code := watch(f, fast(options{repo: "o/r", number: 1, dryRun: true}), &out, nil)

	if code != 0 || len(f.reruns) != 0 {
		t.Fatalf("code=%d reruns=%v, want 0 and none\n%s", code, f.reruns, out.String())
	}
	if !strings.Contains(out.String(), "would re-run failed jobs of CI (7)") {
		t.Errorf("expected a dry-run line:\n%s", out.String())
	}
}

func TestWatchStopsRetryingWhenGitHubRefuses(t *testing.T) {
	failed := rerun.Run{DatabaseID: 7, WorkflowName: "CI",
		Status: "completed", Conclusion: "failure"}
	f := &fakeClient{sha: "abc", polls: [][]rerun.Run{{failed}}, failNext: true}

	var out bytes.Buffer
	code := watch(f, fast(options{repo: "o/r", number: 1, attempts: 3}), &out, nil)

	if code != 1 {
		t.Fatalf("exit code = %d, want 1\n%s", code, out.String())
	}
	// One attempt, then it gives up rather than spinning on a refusal.
	if len(f.reruns) != 1 {
		t.Fatalf("re-runs = %v, want exactly one attempt\n%s", f.reruns, out.String())
	}
	if !strings.Contains(out.String(), "could not re-run") {
		t.Errorf("expected the refusal to be reported:\n%s", out.String())
	}
}

func TestWatchNoticesANewHeadCommit(t *testing.T) {
	ok := rerun.Run{DatabaseID: 1, WorkflowName: "CI",
		Status: "completed", Conclusion: "success"}
	f := &fakeClient{sha: "aaaaaaaaaa", polls: [][]rerun.Run{{ok}}}
	// The head moves between the opening read and the first poll.
	moved := &movingClient{fakeClient: f, second: "bbbbbbbbbb"}

	var out bytes.Buffer
	code := watch(moved, fast(options{repo: "o/r", number: 1}), &out, nil)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "head moved to bbbbbbbbbb") {
		t.Errorf("expected the moved head to be reported:\n%s", out.String())
	}
}

type movingClient struct {
	*fakeClient
	second string
	asked  int
}

func (m *movingClient) pullRequest(repo string, n int) (string, string, string, error) {
	m.asked++
	if m.asked == 1 {
		return m.fakeClient.pullRequest(repo, n)
	}
	return m.second, "open", "A title", nil
}
