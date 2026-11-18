// Package gitinfo describes a local checkout the way captain_utils stamps
// "custom" installs: branch@sha, plus ", dirty" with uncommitted changes under
// the directory. It reads the repository with go-git, so an operator's
// GIT_DIR/GIT_WORK_TREE can never point it at another repo.
package gitinfo

import (
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5"
)

// Describe returns "branch@sha[, dirty]" for dir, or "" when dir is not inside
// a git checkout (or HEAD has no commit). A detached HEAD shows as "HEAD@sha".
func Describe(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	repo, err := git.PlainOpenWithOptions(abs, &git.PlainOpenOptions{DetectDotGit: true, EnableDotGitCommonDir: true})
	if err != nil {
		return ""
	}
	head, err := repo.Head()
	if err != nil {
		return ""
	}
	branch := "HEAD"
	if head.Name().IsBranch() {
		branch = head.Name().Short()
	}
	out := branch + "@" + head.Hash().String()[:7]
	if dirty(repo, abs) {
		out += ", dirty"
	}
	return out
}

// dirty reports uncommitted changes (untracked files included) under dir.
func dirty(repo *git.Repository, dir string) bool {
	wt, err := repo.Worktree()
	if err != nil {
		return false
	}
	st, err := wt.Status()
	if err != nil {
		return false
	}
	root, err := filepath.EvalSymlinks(wt.Filesystem.Root())
	if err != nil {
		root = wt.Filesystem.Root()
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return false
	}
	rel = filepath.ToSlash(rel)
	for path, s := range st {
		if s.Worktree == git.Unmodified && s.Staging == git.Unmodified {
			continue
		}
		if rel == "." || path == rel || strings.HasPrefix(path, rel+"/") {
			return true
		}
	}
	return false
}
