package rerun

import "testing"

func TestParseRef(t *testing.T) {
	t.Run("url", func(t *testing.T) {
		repo, n, err := ParseRef("https://github.com/OWNER/REPO/pull/123", "")
		if err != nil || repo != "OWNER/REPO" || n != 123 {
			t.Fatalf("got %q %d err=%v", repo, n, err)
		}
	})
	t.Run("url with trailing path", func(t *testing.T) {
		repo, n, err := ParseRef("https://github.com/o/r/pull/7/files", "")
		if err != nil || repo != "o/r" || n != 7 {
			t.Fatalf("got %q %d err=%v", repo, n, err)
		}
	})
	t.Run("url with matching repo flag", func(t *testing.T) {
		repo, n, err := ParseRef("https://github.com/o/r/pull/7", "o/r")
		if err != nil || repo != "o/r" || n != 7 {
			t.Fatalf("got %q %d err=%v", repo, n, err)
		}
	})
	t.Run("url conflicting with repo flag", func(t *testing.T) {
		if _, _, err := ParseRef("https://github.com/o/r/pull/7", "x/y"); err == nil {
			t.Fatal("want a conflict error")
		}
	})
	t.Run("bare number takes the repo flag", func(t *testing.T) {
		repo, n, err := ParseRef("123", "o/r")
		if err != nil || repo != "o/r" || n != 123 {
			t.Fatalf("got %q %d err=%v", repo, n, err)
		}
	})
	t.Run("hash number", func(t *testing.T) {
		repo, n, err := ParseRef("#123", "")
		if err != nil || repo != "" || n != 123 {
			t.Fatalf("got %q %d err=%v", repo, n, err)
		}
	})
	t.Run("not a number", func(t *testing.T) {
		if _, _, err := ParseRef("banana", ""); err == nil {
			t.Fatal("want an error")
		}
	})
	t.Run("zero is rejected", func(t *testing.T) {
		if _, _, err := ParseRef("0", "o/r"); err == nil {
			t.Fatal("want an error")
		}
	})
}

func run(id int64, status, conclusion string) Run {
	return Run{DatabaseID: id, WorkflowName: "W", Status: status, Conclusion: conclusion}
}

func TestClassify(t *testing.T) {
	t.Run("a pending run does not hold back a finished one", func(t *testing.T) {
		runs := []Run{
			run(1, "completed", "failure"),
			run(2, "in_progress", ""),
			run(3, "queued", ""),
		}
		b := Classify(runs, map[int64]int{}, 1)
		if len(b.Pending) != 2 {
			t.Fatalf("want 2 pending, got %d", len(b.Pending))
		}
		// The finished, failed run is bucketed apart from the pending
		// ones — so the caller can act on it while they go on.
		if len(b.Rerun) != 1 {
			t.Fatalf("want 1 rerunnable, got %d", len(b.Rerun))
		}
	})

	t.Run("every failing conclusion is re-runnable", func(t *testing.T) {
		runs := []Run{
			run(1, "completed", "failure"),
			run(2, "completed", "timed_out"),
			run(3, "completed", "cancelled"),
		}
		b := Classify(runs, map[int64]int{}, 1)
		if len(b.Rerun) != 3 {
			t.Fatalf("want 3 rerunnable, got %d", len(b.Rerun))
		}
	})

	t.Run("settled conclusions are left alone", func(t *testing.T) {
		runs := []Run{
			run(1, "completed", "success"),
			run(2, "completed", "skipped"),
			run(3, "completed", "neutral"),
		}
		b := Classify(runs, map[int64]int{}, 1)
		if len(b.Rerun)+len(b.Pending)+len(b.Attention)+len(b.Exhausted) != 0 {
			t.Fatalf("want nothing to do, got %+v", b)
		}
	})

	t.Run("action_required needs attention rather than a re-run", func(t *testing.T) {
		b := Classify([]Run{run(1, "completed", "action_required")}, map[int64]int{}, 1)
		if len(b.Attention) != 1 || len(b.Rerun) != 0 {
			t.Fatalf("want 1 needing attention, got %+v", b)
		}
	})

	t.Run("attempts are capped", func(t *testing.T) {
		runs := []Run{run(1, "completed", "failure")}
		b := Classify(runs, map[int64]int{1: 1}, 1)
		if len(b.Rerun) != 0 || len(b.Exhausted) != 1 {
			t.Fatalf("want the run exhausted, got %+v", b)
		}
		b = Classify(runs, map[int64]int{1: 1}, 3)
		if len(b.Rerun) != 1 || len(b.Exhausted) != 0 {
			t.Fatalf("want the run re-runnable with attempts left, got %+v", b)
		}
	})
}

func TestDescribe(t *testing.T) {
	got := Describe(Run{DatabaseID: 42, WorkflowName: "CI"})
	if got != "CI (42)" {
		t.Fatalf("got %q", got)
	}
}
