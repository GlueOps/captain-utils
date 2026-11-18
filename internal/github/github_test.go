package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReleaseTags(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/GlueOps/platform-crds/releases" || r.URL.Query().Get("per_page") != "10" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("auth header %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`[{"tag_name":"v0.1.7","draft":true},{"tag_name":"v0.1.6"},{"tag_name":"v0.1.5"}]`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client(), Token: "tok"}
	tags, err := c.ReleaseTags(context.Background(), "GlueOps/platform-crds", 10)
	if err != nil || strings.Join(tags, " ") != "v0.1.6 v0.1.5" {
		t.Fatalf("%v %v", tags, err)
	}
	if _, err := c.ReleaseTags(context.Background(), "GlueOps/nope", 10); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("want a 404 error, got %v", err)
	}
}

func TestTokenFromGhConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_CONFIG_DIR", dir)
	if err := writeFile(dir+"/hosts.yml", "github.com:\n  oauth_token: from-gh\n  user: x\n"); err != nil {
		t.Fatal(err)
	}
	if got := token(); got != "from-gh" {
		t.Fatalf("token %q", got)
	}
	t.Setenv("GITHUB_TOKEN", "env")
	if got := token(); got != "env" {
		t.Fatalf("env token %q", got)
	}
}
