// Command ghqfresh keeps the default branch of every ghq-cloned GitHub repository
// of one owner up to date, fast-forward only, leaving anything in use alone.
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

func main() {
	os.Exit(run())
}

func run() int {
	owner := flag.String("owner", envOr("GHQFRESH_OWNER", "ochanuco"), "GitHub owner whose clones under ghq are refreshed")
	changed := flag.Bool("changed", false, "refresh only repositories pushed since their last sync (for scheduled runs)")
	fullEvery := flag.Duration("full-every", 24*time.Hour, "with -changed, refresh everything when the last full run is older than this")
	jobs := flag.Int("jobs", 8, "repositories refreshed in parallel")
	timeout := flag.Duration("timeout", 2*time.Minute, "time limit for each git command")
	statePath := flag.String("state", defaultStatePath(), "state file")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	out := newOutput(os.Stdout)

	unlock, err := lock(*statePath + ".lock")
	if err != nil {
		if *changed {
			return 0
		}
		fmt.Fprintln(os.Stderr, "ghqfresh: another run is in progress")
		return 1
	}
	defer unlock()

	local, err := localRepos(ctx, *owner)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ghqfresh:", err)
		return 1
	}
	if len(local) == 0 {
		return 0
	}
	dirs := map[string]string{}
	var names []string
	for _, l := range local {
		dirs[l.name] = l.dir
		names = append(names, l.name)
	}

	token, err := ghToken(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ghqfresh:", err)
		return 1
	}
	remote, err := listRepos(ctx, &http.Client{Timeout: 20 * time.Second}, "https://api.github.com/graphql", token, *owner)
	if errors.Is(err, ErrOffline) && *changed {
		return 0
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ghqfresh:", err)
		return 1
	}

	st, err := loadState(*statePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ghqfresh:", err)
		return 1
	}
	full := !*changed || time.Since(st.LastFull) > *fullEvery
	targets := due(names, remote, st, full)
	if len(targets) == 0 {
		return 0
	}

	r := &Refresher{
		AuthHeader: "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token)),
		// The x-access-token@ userinfo keeps url.<base>.insteadOf rules for
		// https://github.com/ (e.g. a rewrite to SSH) from matching.
		FetchURL: func(slug string) string { return "https://x-access-token@github.com/" + slug + ".git" },
		Timeout:  *timeout,
	}
	results := refreshAll(ctx, r, targets, dirs, remote, *jobs, out)

	failed := 0
	for _, res := range results {
		switch res.Status {
		case StatusOK, StatusUpdated:
			st.Synced[res.Repo] = remote[res.Repo].PushedAt
		case StatusFailed:
			failed++
		}
	}
	if full && ctx.Err() == nil {
		st.LastFull = time.Now()
	}
	if err := st.save(*statePath); err != nil {
		fmt.Fprintln(os.Stderr, "ghqfresh:", err)
		return 1
	}
	out.summary(results)
	if failed > 0 || ctx.Err() != nil {
		return 1
	}
	return 0
}

func refreshAll(ctx context.Context, r *Refresher, targets []string, dirs map[string]string, remote map[string]RemoteRepo, jobs int, out *output) []Result {
	results := make([]Result, len(targets))
	sem := make(chan struct{}, max(jobs, 1))
	var wg sync.WaitGroup
	out.start(len(targets))
	for i, name := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res := r.Refresh(ctx, name, dirs[name], remote[name].DefaultBranch)
			results[i] = res
			out.done(res)
		}()
	}
	wg.Wait()
	out.stop()
	return results
}

type localRepo struct{ name, dir string }

// localRepos lists ghq clones laid out as <root>/github.com/<owner>/<name>.
func localRepos(ctx context.Context, owner string) ([]localRepo, error) {
	b, err := exec.CommandContext(ctx, "ghq", "list", "-p", "github.com/"+owner+"/").Output()
	if err != nil {
		return nil, fmt.Errorf("ghq list: %w", err)
	}
	var repos []localRepo
	for _, p := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		parent := filepath.Dir(p)
		if p == "" || filepath.Base(parent) != owner || filepath.Base(filepath.Dir(parent)) != "github.com" {
			continue
		}
		repos = append(repos, localRepo{name: filepath.Base(p), dir: p})
	}
	return repos, nil
}

func ghToken(ctx context.Context) (string, error) {
	b, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
	token := strings.TrimSpace(string(b))
	if err != nil || token == "" {
		return "", errors.New("gh auth token failed; run 'gh auth login'")
	}
	return token, nil
}

func lock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

func defaultStatePath() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "ghqfresh", "state.json")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// output prints results as they finish. On a terminal it colours them and keeps a
// progress line at the bottom; otherwise (launchd, pipes) it writes timestamped log
// lines for everything but "ok", so an idle scheduled run writes nothing.
type output struct {
	w      io.Writer
	tty    bool
	mu     sync.Mutex
	total  int
	n      int
	frame  int
	ticker *time.Ticker
	quit   chan struct{}
}

func newOutput(f *os.File) *output {
	fi, err := f.Stat()
	return &output{w: f, tty: err == nil && fi.Mode()&os.ModeCharDevice != 0}
}

var frames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func (o *output) start(total int) {
	o.total = total
	if !o.tty {
		return
	}
	fmt.Fprint(o.w, "\x1b[?25l")
	o.ticker = time.NewTicker(100 * time.Millisecond)
	o.quit = make(chan struct{})
	go func() {
		for {
			select {
			case <-o.ticker.C:
				o.mu.Lock()
				o.frame++
				o.bar()
				o.mu.Unlock()
			case <-o.quit:
				return
			}
		}
	}()
}

func (o *output) bar() {
	filled := o.n * 20 / max(o.total, 1)
	fmt.Fprintf(o.w, "\r\x1b[2K%s [%s%s] %d/%d", frames[o.frame%len(frames)],
		strings.Repeat("#", filled), strings.Repeat("-", 20-filled), o.n, o.total)
}

func (o *output) done(r Result) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.n++
	if !o.tty {
		if r.Status != StatusOK {
			fmt.Fprintf(o.w, "%s %s\n", time.Now().Format(time.RFC3339), r)
		}
		return
	}
	colour := map[Status]string{StatusUpdated: "1;32", StatusFailed: "1;31", StatusSkipped: "33", StatusOK: "2"}[r.Status]
	fmt.Fprintf(o.w, "\r\x1b[2K\x1b[%sm%s\x1b[0m\n", colour, r)
	o.bar()
}

func (o *output) stop() {
	if !o.tty {
		return
	}
	o.ticker.Stop()
	close(o.quit)
	o.mu.Lock()
	defer o.mu.Unlock()
	o.bar()
	fmt.Fprint(o.w, "\n\x1b[?25h")
}

func (o *output) summary(results []Result) {
	count := map[Status]int{}
	for _, r := range results {
		count[r.Status]++
	}
	if !o.tty && count[StatusUpdated]+count[StatusFailed]+count[StatusSkipped] == 0 {
		return
	}
	line := fmt.Sprintf("checked %d: %d updated, %d skipped, %d failed",
		len(results), count[StatusUpdated], count[StatusSkipped], count[StatusFailed])
	if o.tty {
		fmt.Fprintln(o.w, line)
		return
	}
	fmt.Fprintf(o.w, "%s %s\n", time.Now().Format(time.RFC3339), line)
}
