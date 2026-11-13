package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestDue(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	remote := map[string]RemoteRepo{
		"pushed": {PushedAt: t0.Add(time.Hour)},
		"quiet":  {PushedAt: t0},
		"new":    {PushedAt: t0},
	}
	st := &State{Synced: map[string]time.Time{"pushed": t0, "quiet": t0}}
	local := []string{"quiet", "pushed", "new", "not-on-github"}

	if got, want := due(local, remote, st, false), []string{"new", "pushed"}; !reflect.DeepEqual(got, want) {
		t.Errorf("changed run: got %v, want %v", got, want)
	}
	if got, want := due(local, remote, st, true), []string{"new", "not-on-github", "pushed", "quiet"}; !reflect.DeepEqual(got, want) {
		t.Errorf("full run: got %v, want %v", got, want)
	}
}

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "state.json")
	st, err := loadState(path)
	if err != nil || len(st.Synced) != 0 {
		t.Fatalf("missing file: %v %v", st, err)
	}
	st.LastFull = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	st.Synced["x"] = st.LastFull
	if err := st.save(path); err != nil {
		t.Fatal(err)
	}
	got, err := loadState(path)
	if err != nil || !got.LastFull.Equal(st.LastFull) || !got.Synced["x"].Equal(st.LastFull) {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestListReposPaginates(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("auth header %q", r.Header.Get("Authorization"))
		}
		var req struct{ Variables map[string]any }
		json.NewDecoder(r.Body).Decode(&req)
		if req.Variables["after"] == nil {
			w.Write([]byte(`{"data":{"repositoryOwner":{"repositories":{
				"nodes":[{"name":"a","pushedAt":"2026-01-01T00:00:00Z","isArchived":false,"defaultBranchRef":{"name":"main"}},
				         {"name":"old","pushedAt":"2026-01-01T00:00:00Z","isArchived":true,"defaultBranchRef":{"name":"main"}}],
				"pageInfo":{"hasNextPage":true,"endCursor":"c1"}}}}}`))
			return
		}
		w.Write([]byte(`{"data":{"repositoryOwner":{"repositories":{
			"nodes":[{"name":"b","pushedAt":"2026-01-02T00:00:00Z","isArchived":false,"defaultBranchRef":{"name":"master"}}],
			"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}`))
	}))
	defer srv.Close()

	repos, err := listRepos(context.Background(), srv.Client(), srv.URL, "tok", "ochanuco")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(repos) != 2 || repos["b"].DefaultBranch != "master" {
		t.Fatalf("calls=%d repos=%+v", calls, repos)
	}
	if _, ok := repos["old"]; ok {
		t.Fatal("archived repo listed")
	}
}

func TestListReposOffline(t *testing.T) {
	_, err := listRepos(context.Background(), &http.Client{Timeout: time.Second}, "http://127.0.0.1:1/graphql", "tok", "x")
	if !errors.Is(err, ErrOffline) {
		t.Fatalf("got %v, want ErrOffline", err)
	}
}
