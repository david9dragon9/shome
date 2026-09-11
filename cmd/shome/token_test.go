package main

import (
	"strings"
	"testing"
)

// A token variable that is set but empty must stop the command, not fall
// through to the owner's credential.
//
// The fallbacks exist so the owner's CLI works on the controller without
// setup. Combined with a token variable that failed to fill in, they turned
// "act as this one account" into "act as a cluster administrator" -- which is
// what happened to an example script whose token extraction silently produced
// nothing, and the run looked like a successful demonstration of isolation.
func TestEmptyTokenIsRefusedRatherThanFallingBackToTheOwner(t *testing.T) {
	t.Setenv("SHOME_TOKEN", "")
	if _, err := authToken(); err == nil {
		t.Fatal("an empty SHOME_TOKEN was accepted; it must be an error")
	}
	if _, err := validToken(); err == nil || !strings.Contains(err.Error(), "set but empty") {
		t.Errorf("validToken error = %v, want it to name the empty variable", err)
	}
	// Whitespace is the same mistake with the same consequence.
	t.Setenv("SHOME_TOKEN", "  \n")
	if _, err := authToken(); err == nil {
		t.Error("a whitespace-only SHOME_TOKEN was accepted")
	}
	// A real value is used as given.
	t.Setenv("SHOME_TOKEN", " alice.abc123 ")
	got, err := authToken()
	if err != nil || got != "alice.abc123" {
		t.Errorf("authToken = %q, %v; want the trimmed token", got, err)
	}
}
