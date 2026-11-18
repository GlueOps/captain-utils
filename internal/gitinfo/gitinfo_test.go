package gitinfo

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func TestDescribe(t *testing.T) {
	dir := t.TempDir()
	if Describe(dir) != "" {
		t.Fatal("not a checkout: want empty")
	}
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "chart"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(p, s string) {
		if err := os.WriteFile(filepath.Join(dir, p), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("chart/Chart.yaml", "name: x\n")
	write("README.md", "x\n")
	wt, _ := repo.Worktree()
	if _, err := wt.Add("."); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("init", &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@example.com", When: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	clean := regexp.MustCompile(`^master@[0-9a-f]{7}$`)
	if got := Describe(filepath.Join(dir, "chart")); !clean.MatchString(got) {
		t.Fatalf("clean: %q", got)
	}
	// a change outside the directory does not make it dirty (git status -- .)
	write("README.md", "changed\n")
	if got := Describe(filepath.Join(dir, "chart")); !clean.MatchString(got) {
		t.Fatalf("change elsewhere: %q", got)
	}
	write("chart/values.yaml", "new: file\n")
	if got := Describe(filepath.Join(dir, "chart")); !regexp.MustCompile(`^master@[0-9a-f]{7}, dirty$`).MatchString(got) {
		t.Fatalf("dirty: %q", got)
	}
	// GIT_DIR pointing elsewhere is ignored
	t.Setenv("GIT_DIR", t.TempDir())
	if got := Describe(filepath.Join(dir, "chart")); got == "" {
		t.Fatal("GIT_DIR must be ignored")
	}
}
