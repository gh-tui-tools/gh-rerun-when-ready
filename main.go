package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
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
	// retryFor is how long requests may keep failing transiently before the
	// watch gives up.
	retryFor time.Duration
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
	o := options{interval: 60 * time.Second, attempts: 1, retryFor: 5 * time.Minute}
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

// transient reports whether a failed request is worth retrying: It never got
// an answer (a timeout or a dropped connection, e.g.), or GitHub failed on its
// own side, or limited the rate. An answer that declines the request isn't
// transient — retrying it would only get the same answer.
func transient(err error) bool {
	var httpErr *api.HTTPError
	if !errors.As(err, &httpErr) {
		return true
	}
	switch {
	case httpErr.StatusCode >= 500, httpErr.StatusCode == http.StatusTooManyRequests:
		return true
	case httpErr.StatusCode == http.StatusForbidden:
		// GitHub's primary and secondary rate limits answer 403 too.
		return httpErr.Headers.Get("Retry-After") != "" ||
			httpErr.Headers.Get("X-RateLimit-Remaining") == "0"
	}
	return false
}

// requestTimeout bounds each API request. A connection can stall without the
// OS ever reporting an error, so without a bound, a request on it would block
// the watch for good. With one, it fails like any other unanswered request,
// and gets retried.
const requestTimeout = 30 * time.Second

// The first wait before retrying a failed read. Each further wait doubles,
// up to the polling interval.
const firstRetryWait = 5 * time.Second

// errInterrupted reports that an interrupt ended a wait between retries.
var errInterrupted = errors.New("interrupted")

// retryingReads retries the reads of a client that fail transiently, so that
// one dropped request doesn't end a watch that otherwise runs for hours. It
// gives up, and returns the last error, once a read has kept failing for
// o.retryFor.
type retryingReads struct {
	client
	o         options
	out       io.Writer
	interrupt <-chan os.Signal
}

func (c retryingReads) pullRequest(repo string, number int) (sha, state, title string, err error) {
	err = c.retry("reading pull request", func() (err error) {
		sha, state, title, err = c.client.pullRequest(repo, number)
		return err
	})
	return sha, state, title, err
}

func (c retryingReads) runsForCommit(repo, sha string) (runs []rerun.Run, err error) {
	err = c.retry("listing workflow runs", func() (err error) {
		runs, err = c.client.runsForCommit(repo, sha)
		return err
	})
	return runs, err
}

func (c retryingReads) retry(what string, read func() error) error {
	since := time.Time{}
	wait := min(firstRetryWait, c.o.interval)
	for {
		err := read()
		if err == nil || !transient(err) {
			return err
		}
		if since.IsZero() {
			since = time.Now()
		}
		if time.Since(since) >= c.o.retryFor {
			logf(c.out, "giving up: %s kept failing for %s", what,
				time.Since(since).Round(time.Second))
			return err
		}
		logf(c.out, "error %s, trying again in %s: %v", what, wait, err)
		if !sleep(wait, c.interrupt) {
			return errInterrupted
		}
		wait = min(2*wait, c.o.interval)
	}
}

// watch polls the pull request until nothing is still going, re-running the
// failed jobs of the runs that need it, and returns the process exit code.
func watch(c client, o options, out io.Writer, interrupt <-chan os.Signal) int {
	c = retryingReads{client: c, o: o, out: out, interrupt: interrupt}
	sha, state, title, err := c.pullRequest(o.repo, o.number)
	if errors.Is(err, errInterrupted) {
		return 130
	}
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
	// 'attempts' counts the re-runs actually performed, per run. 'refused' holds
	// the runs GitHub declined to re-run, and why. A run is left alone once it's
	// in either — but only 'attempts' reflects work that really happened.
	// 'unsent' holds the runs whose re-run request failed transiently, and when
	// that first happened; they're asked again on the next poll.
	attempts := map[int64]int{}
	refused := map[int64]string{}
	unsent := map[int64]time.Time{}

	for {
		latest, _, _, err := c.pullRequest(o.repo, o.number)
		if errors.Is(err, errInterrupted) {
			return 130
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading pull request: %v\n", err)
			return 1
		}
		if latest != sha {
			logf(out, "head moved to %.10s, watching that instead", latest)
			sha = latest
			attempts, refused = map[int64]int{}, map[int64]string{}
			unsent = map[int64]time.Time{}
		}

		runs, err := c.runsForCommit(o.repo, sha)
		if errors.Is(err, errInterrupted) {
			return 130
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error listing workflow runs: %v\n", err)
			return 1
		}
		for _, r := range runs {
			if _, ok := unsent[r.DatabaseID]; ok && rerun.IsPending(r) {
				// The request failed on our side, but GitHub got it: The run
				// is going again, so that re-run counts.
				attempts[r.DatabaseID]++
				delete(unsent, r.DatabaseID)
			}
		}
		budget := make(map[int64]int, len(attempts)+len(refused))
		for id, n := range attempts {
			budget[id] = n
		}
		for id := range refused {
			budget[id] = o.attempts
		}
		b := rerun.Classify(runs, budget, o.attempts)

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
				continue
			}
			logf(out, "re-running failed jobs of %s [%s] (attempt %d)",
				rerun.Describe(r), r.Conclusion, attempt)
			if err := c.rerunFailedJobs(o.repo, r.DatabaseID); err != nil {
				if !transient(err) {
					logf(out, "  could not re-run: %v", err)
					// Do not spin on a run GitHub will not re-run.
					refused[r.DatabaseID] = err.Error()
					continue
				}
				if _, ok := unsent[r.DatabaseID]; !ok {
					unsent[r.DatabaseID] = time.Now()
				}
				if time.Since(unsent[r.DatabaseID]) >= o.retryFor {
					fmt.Fprintf(os.Stderr, "Error re-running failed jobs of %s: %v\n",
						rerun.Describe(r), err)
					return 1
				}
				logf(out, "  could not re-run, trying again: %v", err)
				continue
			}
			delete(unsent, r.DatabaseID)
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
			var declined, spent []rerun.Run
			for _, r := range b.Exhausted {
				if _, no := refused[r.DatabaseID]; no {
					declined = append(declined, r)
				} else {
					spent = append(spent, r)
				}
			}
			if len(declined) > 0 {
				logf(out, "done: %d run(s) GitHub would not re-run", len(declined))
				for _, r := range declined {
					logf(out, "  %s: %s", rerun.Describe(r), refused[r.DatabaseID])
					logf(out, "  %s", r.URL)
				}
			}
			if len(spent) > 0 {
				logf(out, "done: %d run(s) still failing after %d attempt(s);"+
					" raise --attempts to retry more", len(spent), o.attempts)
				for _, r := range spent {
					logf(out, "  %s %s", rerun.Describe(r), r.URL)
				}
			}
			if len(declined) > 0 || len(spent) > 0 {
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

	rest, err := api.NewRESTClient(api.ClientOptions{Timeout: requestTimeout})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating API client: %v\n", err)
		os.Exit(1)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	os.Exit(watch(restClient{api: rest}, o, os.Stdout, sig))
}
