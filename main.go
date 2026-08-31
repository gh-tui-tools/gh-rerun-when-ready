package main

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/cli/go-gh/v2/pkg/repository"
	"github.com/gh-tui-tools/gh-rerun-when-ready/internal/rerun"
)

type options struct {
	help     bool
	number   int
	dryRun   bool
	repo     string
	target   string
	interval time.Duration
	attempts int
	timeout  time.Duration
}

const helpText = `Usage: gh rerun-when-ready [options] <pull-request>

Re-run a pull request's failed CI jobs, as soon as GitHub will allow it.

GitHub refuses to re-run the failed jobs of a workflow run while any of that
run's other jobs are still going. That refusal applies to one run at a time.
So, each workflow run of the head commit is watched separately: Its failed
jobs are re-run as soon as that run itself finishes, whether or not the
commit's other runs have. Runs that failed, timed out, or were cancelled are
the ones re-run.

Arguments:
  <pull-request>          Pull request number or URL

Options:
  -R, --repo OWNER/REPO   Repository, when the pull request is given as a
                          number and the working directory is not a clone of it
  -i, --interval SECONDS  Seconds between checks (default: 60)
  -a, --attempts N        How many times to re-run a run that keeps failing
                          (default: 1)
  -t, --timeout SECONDS   Give up after this long (default: no limit)
  -n, --dry-run           Report what would be re-run, and re-run nothing
  -h, --help              Show this help message
`

// parseArgs parses CLI flags. The first non-flag argument is the pull
// request. Unknown flags are an error, so that a typo does not silently
// become the pull request argument.
func parseArgs(argv []string) (options, error) {
	o := options{interval: 60 * time.Second, attempts: 1}
	needsValue := func(i int, flag string) (string, error) {
		if i+1 >= len(argv) {
			return "", fmt.Errorf("%s requires a value", flag)
		}
		return argv[i+1], nil
	}
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		switch arg {
		case "-h", "--help":
			o.help = true
		case "-n", "--dry-run":
			o.dryRun = true
		case "-R", "--repo":
			v, err := needsValue(i, arg)
			if err != nil {
				return o, err
			}
			o.repo, i = v, i+1
		case "-i", "--interval":
			v, err := needsValue(i, arg)
			if err != nil {
				return o, err
			}
			n, convErr := strconv.Atoi(v)
			if convErr != nil || n <= 0 {
				return o, fmt.Errorf("--interval requires a positive number of seconds")
			}
			o.interval, i = time.Duration(n)*time.Second, i+1
		case "-a", "--attempts":
			v, err := needsValue(i, arg)
			if err != nil {
				return o, err
			}
			n, convErr := strconv.Atoi(v)
			if convErr != nil || n < 1 {
				return o, fmt.Errorf("--attempts requires a number of 1 or more")
			}
			o.attempts, i = n, i+1
		case "-t", "--timeout":
			v, err := needsValue(i, arg)
			if err != nil {
				return o, err
			}
			n, convErr := strconv.Atoi(v)
			if convErr != nil || n < 0 {
				return o, fmt.Errorf("--timeout requires a number of seconds")
			}
			o.timeout, i = time.Duration(n)*time.Second, i+1
		default:
			if len(arg) > 1 && arg[0] == '-' {
				return o, fmt.Errorf("unknown option: %s", arg)
			}
			if o.target != "" {
				return o, fmt.Errorf("unexpected argument: %s", arg)
			}
			o.target = arg
		}
	}
	if !o.help && o.target == "" {
		return o, fmt.Errorf("a pull request number or URL is required")
	}
	return o, nil
}

func logf(out io.Writer, format string, args ...any) {
	fmt.Fprintf(out, "[%s] %s\n", time.Now().Format("15:04:05"),
		fmt.Sprintf(format, args...))
}

// client is the slice of the GitHub API this tool uses, kept small so that
// the polling loop can be reasoned about without a live connection.
type client interface {
	pullRequest(repo string, number int) (sha, state, title string, err error)
	runsForCommit(repo, sha string) ([]rerun.Run, error)
	rerunFailedJobs(repo string, runID int64) error
}

type restClient struct{ api *api.RESTClient }

func (c restClient) pullRequest(repo string, number int) (string, string, string, error) {
	var pr struct {
		Head  struct{ SHA string } `json:"head"`
		State string               `json:"state"`
		Title string               `json:"title"`
	}
	path := fmt.Sprintf("repos/%s/pulls/%d", repo, number)
	if err := c.api.Get(path, &pr); err != nil {
		return "", "", "", err
	}
	return pr.Head.SHA, pr.State, pr.Title, nil
}

func (c restClient) runsForCommit(repo, sha string) ([]rerun.Run, error) {
	var body struct {
		WorkflowRuns []rerun.Run `json:"workflow_runs"`
	}
	path := fmt.Sprintf("repos/%s/actions/runs?head_sha=%s&per_page=100",
		repo, url.QueryEscape(sha))
	if err := c.api.Get(path, &body); err != nil {
		return nil, err
	}
	return body.WorkflowRuns, nil
}

func (c restClient) rerunFailedJobs(repo string, runID int64) error {
	path := fmt.Sprintf("repos/%s/actions/runs/%d/rerun-failed-jobs", repo, runID)
	return c.api.Post(path, nil, nil)
}

// watch polls the pull request until nothing is still going, re-running the
// failed jobs of the runs that need it, and returns the process exit code.
func watch(c client, o options, out io.Writer, interrupt <-chan os.Signal) int {
	sha, state, title, err := c.pullRequest(o.repo, o.number)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading pull request: %v\n", err)
		return 1
	}
	logf(out, "%s#%d (%s): %s", o.repo, o.number, state, title)
	logf(out, "head is %.10s", sha)
	if state != "open" && state != "OPEN" {
		logf(out, "note: this pull request is %s", state)
	}

	deadline := time.Time{}
	if o.timeout > 0 {
		deadline = time.Now().Add(o.timeout)
	}
	attempts := map[int64]int{}

	for {
		latest, _, _, err := c.pullRequest(o.repo, o.number)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading pull request: %v\n", err)
			return 1
		}
		if latest != sha {
			logf(out, "head moved to %.10s, watching that instead", latest)
			sha, attempts = latest, map[int64]int{}
		}

		runs, err := c.runsForCommit(o.repo, sha)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error listing workflow runs: %v\n", err)
			return 1
		}
		b := rerun.Classify(runs, attempts, o.attempts)

		// GitHub's refusal is scoped to a single run: it declines to re-run
		// the failed jobs of a run whose own jobs are still going. But a run
		// that's finished can be re-run while other runs of the same commit
		// are still going. So, each run is acted on as soon as it finishes,
		// rather than after the slowest run of the commit finishes.
		for _, r := range b.Rerun {
			attempt := attempts[r.DatabaseID] + 1
			if o.dryRun {
				logf(out, "would re-run failed jobs of %s [%s]",
					rerun.Describe(r), r.Conclusion)
				attempts[r.DatabaseID] = o.attempts
				continue
			}
			logf(out, "re-running failed jobs of %s [%s] (attempt %d)",
				rerun.Describe(r), r.Conclusion, attempt)
			if err := c.rerunFailedJobs(o.repo, r.DatabaseID); err != nil {
				logf(out, "  could not re-run: %v", err)
				// Do not spin on a run GitHub will not re-run.
				attempts[r.DatabaseID] = o.attempts
				continue
			}
			attempts[r.DatabaseID] = attempt
		}

		switch {
		case len(runs) == 0:
			logf(out, "no workflow runs for this commit yet")
		case len(b.Pending) > 0:
			logf(out, "waiting on %d run(s): %s", len(b.Pending), summarize(b.Pending))
		case len(b.Rerun) > 0:
			if !o.dryRun {
				// Give GitHub a moment to move the re-runs out of "completed".
				if !sleep(min(o.interval, 10*time.Second), interrupt) {
					return 130
				}
				continue
			}
		default:
			for _, r := range b.Attention {
				logf(out, "needs attention: %s [%s] %s",
					rerun.Describe(r), r.Conclusion, r.URL)
			}
			if len(b.Exhausted) > 0 {
				logf(out, "done: %d run(s) still failing after %d attempt(s)",
					len(b.Exhausted), o.attempts)
				for _, r := range b.Exhausted {
					logf(out, "  %s %s", rerun.Describe(r), r.URL)
				}
				return 1
			}
			logf(out, "done: every run for this commit succeeded")
			return 0
		}

		if o.dryRun {
			return 0
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			logf(out, "giving up after %s", o.timeout)
			return 1
		}
		if !sleep(o.interval, interrupt) {
			return 130
		}
	}
}

// sleep waits for d, reporting false if interrupted.
func sleep(d time.Duration, interrupt <-chan os.Signal) bool {
	select {
	case <-interrupt:
		return false
	case <-time.After(d):
		return true
	}
}

func summarize(runs []rerun.Run) string {
	const show = 3
	names := ""
	for i, r := range runs {
		if i == show {
			return names + fmt.Sprintf(" and %d more", len(runs)-show)
		}
		if i > 0 {
			names += ", "
		}
		names += rerun.Describe(r)
	}
	return names
}

func main() {
	o, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error: "+err.Error())
		fmt.Fprint(os.Stderr, "\n"+helpText)
		os.Exit(1)
	}
	if o.help {
		fmt.Print(helpText)
		os.Exit(0)
	}

	repo, number, err := rerun.ParseRef(o.target, o.repo)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error: "+err.Error())
		os.Exit(1)
	}
	if repo == "" {
		current, err := repository.Current()
		if err != nil {
			fmt.Fprintln(os.Stderr,
				"Error: no repository given; pass --repo OWNER/REPO or run inside a clone")
			os.Exit(1)
		}
		repo = current.Owner + "/" + current.Name
	}
	o.repo, o.number = repo, number

	rest, err := api.DefaultRESTClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating API client: %v\n", err)
		os.Exit(1)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	os.Exit(watch(restClient{api: rest}, o, os.Stdout, sig))
}
