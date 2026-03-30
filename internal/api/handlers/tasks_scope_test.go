package handlers

import (
	"testing"

	"github.com/clawvisor/clawvisor/pkg/store"
)

func TestCheckTaskScope_WorkVariantMatchesExact(t *testing.T) {
	// When both google.calendar and google.calendar:work are authorized,
	// a request for google.calendar:work must match the :work entry.
	task := &store.Task{
		AuthorizedActions: []store.TaskAction{
			{Service: "google.calendar", Action: "*", ExpectedUse: "personal calendar"},
			{Service: "google.calendar:work", Action: "*", ExpectedUse: "work calendar"},
		},
	}

	match := CheckTaskScope(task, "google.calendar", "work", "list")
	if !match.InScope {
		t.Fatal("expected request to be in scope")
	}
	if match.MatchedAction.Service != "google.calendar:work" {
		t.Fatalf("expected match on google.calendar:work, got %s", match.MatchedAction.Service)
	}
	if match.MatchedAction.ExpectedUse != "work calendar" {
		t.Fatalf("expected ExpectedUse='work calendar', got %q", match.MatchedAction.ExpectedUse)
	}
}

func TestCheckTaskScope_WorkVariantMatchesExact_ReversedOrder(t *testing.T) {
	// Same test but with :work entry listed first — should still work.
	task := &store.Task{
		AuthorizedActions: []store.TaskAction{
			{Service: "google.calendar:work", Action: "*", ExpectedUse: "work calendar"},
			{Service: "google.calendar", Action: "*", ExpectedUse: "personal calendar"},
		},
	}

	match := CheckTaskScope(task, "google.calendar", "work", "list")
	if !match.InScope {
		t.Fatal("expected request to be in scope")
	}
	if match.MatchedAction.Service != "google.calendar:work" {
		t.Fatalf("expected match on google.calendar:work, got %s", match.MatchedAction.Service)
	}
}

func TestCheckTaskScope_BaseServiceStillMatches(t *testing.T) {
	// A request for the base service (no alias) should still match the base entry.
	task := &store.Task{
		AuthorizedActions: []store.TaskAction{
			{Service: "google.calendar", Action: "*", ExpectedUse: "personal calendar"},
			{Service: "google.calendar:work", Action: "*", ExpectedUse: "work calendar"},
		},
	}

	match := CheckTaskScope(task, "google.calendar", "", "list")
	if !match.InScope {
		t.Fatal("expected request to be in scope")
	}
	if match.MatchedAction.Service != "google.calendar" {
		t.Fatalf("expected match on google.calendar, got %s", match.MatchedAction.Service)
	}
}

func TestCheckTaskScope_AliasFallsBackToBase(t *testing.T) {
	// When only the base service is authorized, an aliased request should
	// still match (fallback behavior).
	task := &store.Task{
		AuthorizedActions: []store.TaskAction{
			{Service: "google.calendar", Action: "*", ExpectedUse: "calendar access"},
		},
	}

	match := CheckTaskScope(task, "google.calendar", "work", "list")
	if !match.InScope {
		t.Fatal("expected aliased request to fall back to base service")
	}
	if match.MatchedAction.Service != "google.calendar" {
		t.Fatalf("expected fallback match on google.calendar, got %s", match.MatchedAction.Service)
	}
}
