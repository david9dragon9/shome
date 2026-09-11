package ctl

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/davidwu/shome/internal/job"
	"github.com/davidwu/shome/internal/store"
)

// A job's credential must be the account's, scoped to the job, and must not
// be the node's own authority: a node forwards requests for whoever is
// running on it, and a request with no token must get nowhere.
func TestJobAPIAnswersTheJobsTokenAndNothingElse(t *testing.T) {
	c, st := dashFixture(t)
	ctx := context.Background()
	if _, err := st.CreateUser(ctx, "alice", store.RoleUser, 0, c.now()); err != nil {
		t.Fatal(err)
	}
	j := &job.Job{ID: 42, Spec: job.Spec{User: "alice",
		Limits: job.Limits{Walltime: time.Hour}}}
	tok := c.jobToken(ctx, j)
	if tok == "" {
		t.Fatal("no credential was issued for the job")
	}

	srv := httptest.NewServer((&API{c: c}).routes())
	defer srv.Close()

	// With the job's token: answered as that account.
	req, _ := http.NewRequest("GET", srv.URL+"/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a job's own token was refused: %s", resp.Status)
	}

	// Without one: refused. This is what stops a node that can forward from
	// being a node that can ask anything it likes.
	bare, _ := http.NewRequest("GET", srv.URL+"/whoami", nil)
	resp2, err := srv.Client().Do(bare)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("an unauthenticated request got %s, want 401", resp2.Status)
	}
}

// The credential must not outlive the job. Eight places finish a job; the
// sweep is what makes forgetting one of them harmless.
func TestFinishedJobsLoseTheirCredential(t *testing.T) {
	c, st := dashFixture(t)
	ctx := context.Background()
	if _, err := st.CreateUser(ctx, "alice", store.RoleUser, 0, c.now()); err != nil {
		t.Fatal(err)
	}
	j, err := st.Submit(ctx, job.Spec{User: "alice", Name: "j",
		Limits: job.Limits{CPUs: 1, Walltime: time.Hour}}, c.now())
	if err != nil {
		t.Fatal(err)
	}
	tok := c.jobToken(ctx, j)
	if tok == "" {
		t.Fatal("no credential was issued")
	}
	if _, err := st.UserBySessionToken(ctx, tok, c.now()); err != nil {
		t.Fatalf("the credential does not work while the job is queued: %v", err)
	}
	// Still valid once it is running: this is when it is used.
	if err := st.MarkRunning(ctx, j.ID, "mini", "", c.now()); err != nil {
		t.Fatal(err)
	}
	c.reapJobTokens(ctx)
	if _, err := st.UserBySessionToken(ctx, tok, c.now()); err != nil {
		t.Fatalf("a running job's credential was swept away: %v", err)
	}

	if err := st.MarkFinished(ctx, j.ID, job.Completed, 0, "", 0, c.now()); err != nil {
		t.Fatal(err)
	}
	c.reapJobTokens(ctx)
	if _, err := st.UserBySessionToken(ctx, tok, c.now()); err == nil {
		t.Error("a finished job's credential still works")
	}
}

// A job with no account gets no credential: there would be nobody to scope
// it to, and a token scoped to nobody is either useless or dangerous.
func TestNoCredentialWithoutAnAccount(t *testing.T) {
	c, _ := dashFixture(t)
	if tok := c.jobToken(context.Background(), &job.Job{ID: 1}); tok != "" {
		t.Error("a job with no account was given a credential")
	}
}
