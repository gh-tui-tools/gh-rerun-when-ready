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
	sha    string
	polls  [][]rerun.Run
	call   int
	reruns []int64
	// rerunPoll[i] is the poll number reruns[i] was requested during.
	rerunPoll []int
	failNext  bool
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
	f.rerunPoll = append(f.rerunPoll, f.call)
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

func TestWatchRerunsAFinishedRunWhileAnotherIsStillPending(t *testing.T) {
	// GitHub scopes its refusal to a single run. So, a CI run that's
	// finished and failed can be re-run even though the unrelated Flatpak
	// run of the same commit is still going.
	failedCI := rerun.Run{DatabaseID: 7, WorkflowName: "CI",
		Status: "completed", Conclusion: "failure"}
	rerunningCI := rerun.Run{DatabaseID: 7, WorkflowName: "CI",
		Status: "in_progress"}
	greenCI := rerun.Run{DatabaseID: 7, WorkflowName: "CI",
		Status: "completed", Conclusion: "success"}
	pendingFlatpak := rerun.Run{DatabaseID: 8, WorkflowName: "Flatpak",
		Status: "in_progress"}
	greenFlatpak := rerun.Run{DatabaseID: 8, WorkflowName: "Flatpak",
		Status: "completed", Conclusion: "success"}

	f := &fakeClient{sha: "548b670015", polls: [][]rerun.Run{
		{failedCI, pendingFlatpak},    // CI has finished, Flatpak has not
		{rerunningCI, pendingFlatpak}, // the re-run is under way
		{greenCI, greenFlatpak},
	}}

	var out bytes.Buffer
	code := watch(f, fast(options{repo: "o/r", number: 1}), &out, nil)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out.String())
	}
	if len(f.reruns) != 1 || f.reruns[0] != 7 {
		t.Fatalf("re-runs = %v, want exactly [7]\n%s", f.reruns, out.String())
	}
	if f.rerunPoll[0] != 1 {
		t.Errorf("re-ran during poll %d, want poll 1: Flatpak being unfinished"+
			" must not hold CI back\n%s", f.rerunPoll[0], out.String())
	}
	if !strings.Contains(out.String(), "waiting on 1 run(s): Flatpak") {
		t.Errorf("expected it to go on waiting for Flatpak:\n%s", out.String())
	}
}

func TestWatchWaitsForARunToFinishBeforeRerunningIt(t *testing.T) {
	// The refusal that does bite: A run whose own jobs are still going
	// can't have its failed jobs re-run yet.
	running := rerun.Run{DatabaseID: 7, WorkflowName: "CI",
		Status: "in_progress"}
	failed := rerun.Run{DatabaseID: 7, WorkflowName: "CI",
		Status: "completed", Conclusion: "failure"}
	green := rerun.Run{DatabaseID: 7, WorkflowName: "CI",
		Status: "completed", Conclusion: "success"}

	f := &fakeClient{sha: "abc1234567", polls: [][]rerun.Run{
		{running}, // still going, so there's nothing to do yet
		{failed},  // finished and failed, so re-run it now
		{green},
	}}

	var out bytes.Buffer
	code := watch(f, fast(options{repo: "o/r", number: 1}), &out, nil)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out.String())
	}
	if len(f.reruns) != 1 || f.reruns[0] != 7 {
		t.Fatalf("re-runs = %v, want exactly [7]\n%s", f.reruns, out.String())
	}
	if f.rerunPoll[0] != 2 {
		t.Errorf("re-ran during poll %d, want poll 2: it was still going at"+
			" poll 1\n%s", f.rerunPoll[0], out.String())
	}
	if !strings.Contains(out.String(), "every run for this commit succeeded") {
		t.Errorf("expected a success line:\n%s", out.String())
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
	log := out.String()
	if !strings.Contains(log, "could not re-run") {
		t.Errorf("expected the refusal to be reported:\n%s", log)
	}
	if !strings.Contains(log, "GitHub would not re-run") {
		t.Errorf("expected the summary to name the refusal:\n%s", log)
	}
	// A refusal isn't a spent retry budget — and must not read as one.
	if strings.Contains(log, "still failing after") {
		t.Errorf("a refusal reported as a spent budget:\n%s", log)
	}
}

func TestWatchReportsASpentAttemptBudgetSeparately(t *testing.T) {
	// The other way a run ends up unfixed: It really was re-run, and really
	// failed again. That must not read like a refusal.
	failed := rerun.Run{DatabaseID: 7, WorkflowName: "CI",
		Status: "completed", Conclusion: "failure"}
	f := &fakeClient{sha: "abc", polls: [][]rerun.Run{{failed}}}

	var out bytes.Buffer
	code := watch(f, fast(options{repo: "o/r", number: 1}), &out, nil)

	if code != 1 {
		t.Fatalf("exit code = %d, want 1\n%s", code, out.String())
	}
	if len(f.reruns) != 1 {
		t.Fatalf("re-runs = %v, want exactly one\n%s", f.reruns, out.String())
	}
	log := out.String()
	if !strings.Contains(log, "still failing after 1 attempt(s)") {
		t.Errorf("expected the spent budget to be reported:\n%s", log)
	}
	if !strings.Contains(log, "raise --attempts") {
		t.Errorf("expected it to say how to retry more:\n%s", log)
	}
	// The re-run did happen — so this must not read as a refusal.
	if strings.Contains(log, "GitHub would not re-run") {
		t.Errorf("a spent budget reported as a refusal:\n%s", log)
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
