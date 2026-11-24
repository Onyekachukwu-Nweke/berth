package usecase

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRepoFiles_CommitsEachFile(t *testing.T) {
	repo := &fakeRepoWriter{}
	svc := NewRepoFileService(fakeTokenStore{token: Token{AccessToken: "tok"}}, repo)
	err := svc.Commit(context.Background(), CommitFilesInput{
		Owner: "acme", Repo: "shop", Branch: "main",
		Files: []RepoFile{{Path: "Dockerfile", Content: "FROM node"}, {Path: "docker-compose.yml", Content: "services: {}"}},
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if len(repo.calls) != 2 {
		t.Fatalf("want 2 writes, got %d", len(repo.calls))
	}
}

func TestRepoFiles_RefusesUnsafeFiles(t *testing.T) {
	svc := NewRepoFileService(fakeTokenStore{token: Token{AccessToken: "tok"}}, &fakeRepoWriter{})
	big := strings.Repeat("x", maxCommitFileBytes+1)
	for name, files := range map[string][]RepoFile{
		"none":      nil,
		"absolute":  {{Path: "/etc/passwd", Content: "x"}},
		"traversal": {{Path: "a/../../b", Content: "x"}},
		"workflow":  {{Path: ".github/workflows/x.yml", Content: "x"}},
		"duplicate": {{Path: "Dockerfile"}, {Path: "Dockerfile"}},
		"too big":   {{Path: "Dockerfile", Content: big}},
		"odd chars": {{Path: "Docker file", Content: "x"}},
	} {
		err := svc.Commit(context.Background(), CommitFilesInput{Owner: "acme", Repo: "shop", Branch: "main", Files: files})
		if !errors.Is(err, ErrInvalidFiles) {
			t.Errorf("%s: err = %v, want ErrInvalidFiles", name, err)
		}
	}
}
