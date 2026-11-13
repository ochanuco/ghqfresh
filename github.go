package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

type RemoteRepo struct {
	PushedAt      time.Time
	DefaultBranch string
}

// ErrOffline means GitHub could not be reached at all (no network, DNS down, just
// woke from sleep). The caller treats it as "nothing to do this round".
var ErrOffline = errors.New("github.com is unreachable")

const reposQuery = `query($owner: String!, $after: String) {
  repositoryOwner(login: $owner) {
    repositories(first: 100, after: $after, isFork: false) {
      nodes { name pushedAt isArchived defaultBranchRef { name } }
      pageInfo { hasNextPage endCursor }
    }
  }
}`

// listRepos returns every non-archived repository of owner with its last push time
// and default branch, in one paginated GraphQL query.
func listRepos(ctx context.Context, client *http.Client, endpoint, token, owner string) (map[string]RemoteRepo, error) {
	repos := map[string]RemoteRepo{}
	var after *string
	for {
		body, _ := json.Marshal(map[string]any{
			"query":     reposQuery,
			"variables": map[string]any{"owner": owner, "after": after},
		})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			var netErr net.Error
			var dnsErr *net.DNSError
			var opErr *net.OpError
			if errors.As(err, &dnsErr) || errors.As(err, &opErr) || (errors.As(err, &netErr) && netErr.Timeout()) {
				return nil, ErrOffline
			}
			return nil, err
		}
		var page struct {
			Data struct {
				RepositoryOwner *struct {
					Repositories struct {
						Nodes []struct {
							Name             string
							PushedAt         time.Time
							IsArchived       bool
							DefaultBranchRef *struct{ Name string }
						}
						PageInfo struct {
							HasNextPage bool
							EndCursor   string
						}
					}
				}
			}
			Errors []struct{ Message string }
		}
		err = json.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("github graphql: %s", resp.Status)
		}
		if err != nil {
			return nil, fmt.Errorf("github graphql: %w", err)
		}
		if len(page.Errors) > 0 {
			return nil, fmt.Errorf("github graphql: %s", page.Errors[0].Message)
		}
		if page.Data.RepositoryOwner == nil {
			return nil, fmt.Errorf("github graphql: owner %q not found", owner)
		}
		conn := page.Data.RepositoryOwner.Repositories
		for _, n := range conn.Nodes {
			if n.IsArchived {
				continue
			}
			r := RemoteRepo{PushedAt: n.PushedAt}
			if n.DefaultBranchRef != nil {
				r.DefaultBranch = n.DefaultBranchRef.Name
			}
			repos[n.Name] = r
		}
		if !conn.PageInfo.HasNextPage {
			return repos, nil
		}
		cursor := conn.PageInfo.EndCursor
		after = &cursor
	}
}
