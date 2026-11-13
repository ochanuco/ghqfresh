package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Status int

const (
	StatusOK Status = iota
	StatusUpdated
	StatusSkipped
	StatusFailed
)

func (s Status) String() string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusUpdated:
		return "updated"
	case StatusSkipped:
		return "skip"
	default:
		return "FAIL"
	}
}

type Result struct {
	Repo   string
	Status Status
	Detail string
}

func (r Result) String() string {
	return fmt.Sprintf("%-7s %s (%s)", r.Status, r.Repo, r.Detail)
}

// Refresher brings one clone's default branch up to date with its GitHub origin.
type Refresher struct {
	// AuthHeader is sent as http.extraHeader through GIT_CONFIG_* env vars, so the
	// token never lands in argv or .git/config.
	AuthHeader string
	// FetchURL maps an owner/name slug to the URL fetched from; tests point it at a local repo.
	FetchURL func(slug string) string
	Timeout  time.Duration
}

func (r *Refresher) git(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	if r.AuthHeader != "" {
		cmd.Env = append(cmd.Env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0="+r.AuthHeader)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("git %s timed out after %s", args[0], r.Timeout)
	}
	text := strings.TrimSpace(out.String())
	if err != nil {
		if text == "" {
			text = err.Error()
		}
		first, _, _ := strings.Cut(text, "\n")
		return "", errors.New(first)
	}
	return text, nil
}

// Refresh fetches branch from origin and fast-forwards the local branch. When branch
// is empty it falls back to origin/HEAD.
func (r *Refresher) Refresh(ctx context.Context, repo, dir, branch string) Result {
	res := func(s Status, format string, a ...any) Result {
		return Result{Repo: repo, Status: s, Detail: fmt.Sprintf(format, a...)}
	}

	origin, err := r.git(ctx, dir, "remote", "get-url", "origin")
	if err != nil {
		return res(StatusSkipped, "no origin")
	}
	slug, ok := githubSlug(origin)
	if !ok {
		return res(StatusSkipped, "origin is not on github.com")
	}
	if branch == "" {
		head, err := r.git(ctx, dir, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD")
		if err != nil {
			return res(StatusFailed, "default branch unknown")
		}
		branch = strings.TrimPrefix(head, "origin/")
	}

	localRef := "refs/heads/" + branch
	remoteRef := "refs/remotes/origin/" + branch
	before, _ := r.git(ctx, dir, "rev-parse", "--verify", "--quiet", localRef)

	if _, err := r.git(ctx, dir, "fetch", "--quiet", "--no-tags", r.FetchURL(slug), "+"+localRef+":"+remoteRef); err != nil {
		return res(StatusFailed, "%s: %v", branch, err)
	}
	if before != "" {
		if _, err := r.git(ctx, dir, "merge-base", "--is-ancestor", before, remoteRef); err != nil {
			return res(StatusSkipped, "%s has diverged from origin", branch)
		}
	}

	wt, err := r.checkedOutAt(ctx, dir, localRef)
	if err != nil {
		return res(StatusFailed, "%s: %v", branch, err)
	}
	if wt == "" {
		// Not checked out anywhere: move the ref only. Without a leading + the update
		// is refused unless it is a fast-forward.
		if _, err := r.git(ctx, dir, "fetch", "--quiet", ".", remoteRef+":"+localRef); err != nil {
			return res(StatusFailed, "%s: %v", branch, err)
		}
	} else {
		if busy := r.busy(ctx, wt); busy != "" {
			return res(StatusSkipped, "%s %s", branch, busy)
		}
		if _, err := r.git(ctx, wt, "merge", "--ff-only", "--quiet", remoteRef); err != nil {
			return res(StatusFailed, "%s: %v", branch, err)
		}
	}

	after, _ := r.git(ctx, dir, "rev-parse", "--verify", "--quiet", localRef)
	switch {
	case before == after:
		return res(StatusOK, "%s", branch)
	case before == "":
		return res(StatusUpdated, "%s, new", branch)
	default:
		n, _ := r.git(ctx, dir, "rev-list", "--count", before+".."+after)
		count, _ := strconv.Atoi(n)
		unit := "commits"
		if count == 1 {
			unit = "commit"
		}
		return res(StatusUpdated, "%s, +%d %s", branch, count, unit)
	}
}

// checkedOutAt returns the worktree that has ref checked out, or "" when none does.
func (r *Refresher) checkedOutAt(ctx context.Context, dir, ref string) (string, error) {
	out, err := r.git(ctx, dir, "worktree", "list", "--porcelain")
	if err != nil {
		return "", err
	}
	return parseWorktreeBranch(out, ref), nil
}

func parseWorktreeBranch(porcelain, ref string) string {
	var path string
	for _, line := range strings.Split(porcelain, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			path = strings.TrimPrefix(line, "worktree ")
		case line == "branch "+ref:
			return path
		}
	}
	return ""
}

// busy reports why the worktree must not be touched, or "" when it is safe to fast-forward.
func (r *Refresher) busy(ctx context.Context, wt string) string {
	gitDir, err := r.git(ctx, wt, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "worktree is unreadable"
	}
	for _, name := range []string{"index.lock", "rebase-merge", "rebase-apply", "MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "BISECT_LOG"} {
		if _, err := os.Stat(filepath.Join(gitDir, name)); err == nil {
			return "has a git operation in progress (" + name + ")"
		}
	}
	status, err := r.git(ctx, wt, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return "worktree is unreadable"
	}
	if status != "" {
		return "has local changes"
	}
	return ""
}

func githubSlug(origin string) (string, bool) {
	s := strings.TrimSuffix(origin, ".git")
	for _, prefix := range []string{"git@github.com:", "ssh://git@github.com/", "https://github.com/"} {
		if rest, ok := strings.CutPrefix(s, prefix); ok && strings.Count(rest, "/") == 1 {
			return rest, true
		}
	}
	return "", false
}
