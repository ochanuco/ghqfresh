package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sh(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func commit(t *testing.T, dir, file string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(file), 0o644); err != nil {
		t.Fatal(err)
	}
	sh(t, dir, "add", file)
	sh(t, dir, "commit", "-q", "-m", file)
}

// fixture builds an "upstream" repo and a clone of it whose origin looks like GitHub,
// with FetchURL redirected to the upstream directory.
func fixture(t *testing.T) (upstream, clone string, r *Refresher) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	root := t.TempDir()
	upstream = filepath.Join(root, "upstream")
	clone = filepath.Join(root, "clone")
	os.Mkdir(upstream, 0o755)
	sh(t, upstream, "init", "-q", "-b", "main")
	commit(t, upstream, "a")
	sh(t, root, "clone", "-q", upstream, clone)
	sh(t, clone, "remote", "set-url", "origin", "git@github.com:ochanuco/example.git")
	r = &Refresher{FetchURL: func(string) string { return upstream }, Timeout: 30 * time.Second}
	return upstream, clone, r
}

func TestRefreshCheckedOutClean(t *testing.T) {
	upstream, clone, r := fixture(t)
	commit(t, upstream, "b")
	commit(t, upstream, "c")
	res := r.Refresh(context.Background(), "example", clone, "main")
	if res.Status != StatusUpdated || res.Detail != "main, +2 commits" {
		t.Fatalf("got %v", res)
	}
	if got, want := sh(t, clone, "rev-parse", "HEAD"), sh(t, upstream, "rev-parse", "HEAD"); got != want {
		t.Fatalf("HEAD %s, want %s", got, want)
	}
	if res := r.Refresh(context.Background(), "example", clone, "main"); res.Status != StatusOK {
		t.Fatalf("second run: %v", res)
	}
}

func TestRefreshBranchNotCheckedOut(t *testing.T) {
	upstream, clone, r := fixture(t)
	sh(t, clone, "switch", "-q", "-c", "feature")
	commit(t, upstream, "b")
	res := r.Refresh(context.Background(), "example", clone, "main")
	if res.Status != StatusUpdated {
		t.Fatalf("got %v", res)
	}
	if got := sh(t, clone, "branch", "--show-current"); got != "feature" {
		t.Fatalf("current branch changed to %s", got)
	}
}

func TestRefreshMainInOtherWorktree(t *testing.T) {
	upstream, clone, r := fixture(t)
	sh(t, clone, "switch", "-q", "-c", "feature")
	wt := filepath.Join(t.TempDir(), "wt")
	sh(t, clone, "worktree", "add", "-q", wt, "main")
	commit(t, upstream, "b")
	res := r.Refresh(context.Background(), "example", clone, "main")
	if res.Status != StatusUpdated {
		t.Fatalf("got %v", res)
	}
	if got, want := sh(t, wt, "rev-parse", "HEAD"), sh(t, upstream, "rev-parse", "HEAD"); got != want {
		t.Fatalf("worktree HEAD %s, want %s", got, want)
	}
	if st := sh(t, wt, "status", "--porcelain"); st != "" {
		t.Fatalf("worktree left dirty: %s", st)
	}
}

func TestRefreshSkipsLocalChanges(t *testing.T) {
	upstream, clone, r := fixture(t)
	os.WriteFile(filepath.Join(clone, "a"), []byte("edited"), 0o644)
	commit(t, upstream, "b")
	before := sh(t, clone, "rev-parse", "HEAD")
	res := r.Refresh(context.Background(), "example", clone, "main")
	if res.Status != StatusSkipped || res.Detail != "main has local changes" {
		t.Fatalf("got %v", res)
	}
	if sh(t, clone, "rev-parse", "HEAD") != before {
		t.Fatal("HEAD moved")
	}
}

func TestRefreshSkipsOperationInProgress(t *testing.T) {
	upstream, clone, r := fixture(t)
	commit(t, upstream, "b")
	os.WriteFile(filepath.Join(clone, ".git", "index.lock"), nil, 0o644)
	res := r.Refresh(context.Background(), "example", clone, "main")
	if res.Status != StatusSkipped || !strings.Contains(res.Detail, "index.lock") {
		t.Fatalf("got %v", res)
	}
}

func TestRefreshSkipsDiverged(t *testing.T) {
	upstream, clone, r := fixture(t)
	commit(t, upstream, "b")
	commit(t, clone, "local")
	res := r.Refresh(context.Background(), "example", clone, "main")
	if res.Status != StatusSkipped || res.Detail != "main has diverged from origin" {
		t.Fatalf("got %v", res)
	}
}

func TestRefreshNonGitHubOrigin(t *testing.T) {
	_, clone, r := fixture(t)
	sh(t, clone, "remote", "set-url", "origin", "https://gitlab.com/x/y.git")
	if res := r.Refresh(context.Background(), "example", clone, "main"); res.Status != StatusSkipped {
		t.Fatalf("got %v", res)
	}
}

func TestGithubSlug(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:ochanuco/ghqfresh.git":      "ochanuco/ghqfresh",
		"ssh://git@github.com/ochanuco/ghqfresh":    "ochanuco/ghqfresh",
		"https://github.com/ochanuco/ghqfresh.git":  "ochanuco/ghqfresh",
		"https://gitlab.com/ochanuco/ghqfresh.git":  "",
		"https://github.com/ochanuco/ghqfresh/wiki": "",
	} {
		got, _ := githubSlug(in)
		if got != want {
			t.Errorf("githubSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseWorktreeBranch(t *testing.T) {
	porcelain := "worktree /a\nHEAD 1\nbranch refs/heads/feature\n\nworktree /b\nHEAD 2\nbranch refs/heads/main\n\nworktree /c\nHEAD 3\ndetached\n"
	if got := parseWorktreeBranch(porcelain, "refs/heads/main"); got != "/b" {
		t.Fatalf("got %q", got)
	}
	if got := parseWorktreeBranch(porcelain, "refs/heads/master"); got != "" {
		t.Fatalf("got %q", got)
	}
}
