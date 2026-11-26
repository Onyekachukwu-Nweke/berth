package usecase

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// RepoFile is one file to write into a repo.
type RepoFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// CommitFilesInput writes a handful of files (a Docker setup from a template)
// onto a branch.
type CommitFilesInput struct {
	Owner   string
	Repo    string
	Branch  string
	Message string
	Files   []RepoFile
}

const (
	maxCommitFiles     = 10
	maxCommitFileBytes = 200 << 10
)

// safeRepoPath is a plain relative path: no leading slash, no "..", no spaces
// or shell-ish characters.
var safeRepoPath = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`)

// ErrInvalidFiles is returned when a commit request names bad files.
var ErrInvalidFiles = errors.New("invalid files")

// RepoFileService commits files into a repo with the GitHub App, so people can
// add a Docker setup without cloning. The route requires push access.
type RepoFileService struct {
	tokens RepoTokens
	repo   RepoWriter
}

// NewRepoFileService wires the service to the App's tokens and the writer.
func NewRepoFileService(tokens RepoTokens, repo RepoWriter) *RepoFileService {
	return &RepoFileService{tokens: tokens, repo: repo}
}

func validateCommit(in CommitFilesInput) error {
	if strings.TrimSpace(in.Branch) == "" {
		return fmt.Errorf("%w: a branch is required", ErrInvalidFiles)
	}
	if len(in.Files) == 0 || len(in.Files) > maxCommitFiles {
		return fmt.Errorf("%w: commit between 1 and %d files", ErrInvalidFiles, maxCommitFiles)
	}
	seen := map[string]bool{}
	for _, f := range in.Files {
		p := f.Path
		if !safeRepoPath.MatchString(p) || strings.Contains(p, "..") {
			return fmt.Errorf("%w: %q is not a plain relative path", ErrInvalidFiles, p)
		}
		// Workflows are berth's to write, from the deploy flow.
		if strings.HasPrefix(strings.ToLower(p), ".github/") {
			return fmt.Errorf("%w: %q: files under .github are managed by deploys", ErrInvalidFiles, p)
		}
		if seen[p] {
			return fmt.Errorf("%w: %q is listed twice", ErrInvalidFiles, p)
		}
		seen[p] = true
		if len(f.Content) > maxCommitFileBytes {
			return fmt.Errorf("%w: %q is larger than %d KB", ErrInvalidFiles, p, maxCommitFileBytes>>10)
		}
	}
	return nil
}

// Commit writes every file onto the branch, one commit per file (the contents
// API's unit). A file whose content is already identical is left alone.
func (s *RepoFileService) Commit(ctx context.Context, in CommitFilesInput) error {
	if err := validateCommit(in); err != nil {
		return err
	}
	token, err := s.tokens.TokenFor(ctx, in.Owner)
	if err != nil {
		return err
	}
	for _, f := range in.Files {
		msg := strings.TrimSpace(in.Message)
		if msg == "" {
			msg = "berth: add " + f.Path
		} else {
			msg = msg + " (" + f.Path + ")"
		}
		if err := s.repo.PutFile(ctx, token, in.Owner, in.Repo, in.Branch, f.Path, f.Content, msg); err != nil {
			return err
		}
	}
	return nil
}
