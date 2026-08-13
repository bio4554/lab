package kbased

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/bio4554/lab/internal/kbclient"
)

// wantTicketConflict asserts err is a 409 APIError carrying the
// ticket's current state, and returns that state.
func wantTicketConflict(t *testing.T, err error) kbclient.Ticket {
	t.Helper()
	var apiErr *kbclient.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict {
		t.Fatalf("want 409 APIError, got %v", err)
	}
	if apiErr.Ticket == nil {
		t.Fatalf("409 without current ticket state: %v", err)
	}
	return *apiErr.Ticket
}

// TestTicketLifecycle drives create → claim → start → comment → done,
// asserting the CAS counter, the status chain and per-step provenance
// in the entry history; then the abandon path ends claimable-again by
// a second principal.
func TestTicketLifecycle(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	proj := uuid.New()
	impl1 := f.principal(t, "agent", "impl1")
	impl2 := f.principal(t, "agent", "impl2")
	c1 := f.client(f.token(t, impl1.ID, &proj))
	c2 := f.client(f.token(t, impl2.ID, &proj))

	ticket, err := c1.CreateTicket(ctx, kbclient.CreateTicketRequest{
		Title: "Fix the flaky test", Body: "It fails on Tuesdays.",
	})
	if err != nil {
		t.Fatalf("create ticket: %v", err)
	}
	if ticket.Status != "open" || ticket.CASVersion != 0 || ticket.ClaimedBy != nil {
		t.Fatalf("fresh ticket = %+v", ticket)
	}
	if ticket.Slug != "fix-the-flaky-test" {
		t.Fatalf("slug = %q", ticket.Slug)
	}

	// Bare ticket entries are rejected — the entry+row pair is atomic.
	_, err = c1.CreateEntry(ctx, kbclient.CreateEntryRequest{
		Type: "ticket", Title: "orphan", Content: "x",
	})
	wantAPIError(t, err, http.StatusBadRequest)

	ticket, err = c1.ClaimTicket(ctx, ticket.ID.String(), 0)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if ticket.Status != "claimed" || ticket.CASVersion != 1 ||
		ticket.ClaimedBy == nil || ticket.ClaimedBy.ID != impl1.ID {
		t.Fatalf("after claim = %+v", ticket)
	}

	// Stale CAS: replaying the old version is a 409 and changes nothing.
	_, serr := c1.StartTicket(ctx, ticket.Slug, 0)
	current := wantTicketConflict(t, serr)
	if current.Status != "claimed" || current.CASVersion != 1 {
		t.Fatalf("state after stale CAS = %+v", current)
	}

	// Non-claimant transitions are conflicts too.
	_, serr = c2.StartTicket(ctx, ticket.Slug, 1)
	wantTicketConflict(t, serr)

	ticket, err = c1.StartTicket(ctx, ticket.Slug, 1)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if ticket.Status != "in_progress" || ticket.CASVersion != 2 {
		t.Fatalf("after start = %+v", ticket)
	}

	// Comments append a version without touching status or CAS. A
	// second principal may comment (it's a write in the project).
	if _, err := c2.CommentTicket(ctx, ticket.Slug, "Repro'd on CI."); err != nil {
		t.Fatalf("comment: %v", err)
	}

	ticket, err = c1.DoneTicket(ctx, ticket.Slug, 2)
	if err != nil {
		t.Fatalf("done: %v", err)
	}
	if ticket.Status != "done" || ticket.CASVersion != 3 {
		t.Fatalf("after done = %+v", ticket)
	}

	// History: v1 creation by impl1, then claim, start (impl1), comment
	// (impl2), done (impl1) — five versions, each with the acting
	// principal.
	entry, err := c1.GetEntry(ctx, ticket.Slug, kbclient.GetEntryOptions{History: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(entry.History) != 5 {
		t.Fatalf("history has %d versions, want 5", len(entry.History))
	}
	wantAuthors := []uuid.UUID{impl1.ID, impl2.ID, impl1.ID, impl1.ID, impl1.ID} // newest first
	for i, v := range entry.History {
		if v.Author.ID != wantAuthors[i] {
			t.Fatalf("history[%d] author = %s, want %s", i, v.Author.ID, wantAuthors[i])
		}
	}
	for _, marker := range []string{
		"It fails on Tuesdays.",
		"_claimed by agent:impl1 — ",
		"_started by agent:impl1 — ",
		"_comment by agent:impl2 — ",
		"Repro'd on CI.",
		"_completed by agent:impl1 — ",
	} {
		if !strings.Contains(entry.Content, marker) {
			t.Fatalf("final content missing %q:\n%s", marker, entry.Content)
		}
	}

	// Abandon path: a fresh ticket claimed by impl1, abandoned, then
	// successfully re-claimed by impl2.
	tk2, err := c1.CreateTicket(ctx, kbclient.CreateTicketRequest{
		Title: "Second ticket", Body: "body",
	})
	if err != nil {
		t.Fatal(err)
	}
	if tk2, err = c1.ClaimTicket(ctx, tk2.ID.String(), 0); err != nil {
		t.Fatal(err)
	}
	if tk2, err = c1.AbandonTicket(ctx, tk2.ID.String(), tk2.CASVersion); err != nil {
		t.Fatal(err)
	}
	if tk2.Status != "abandoned" || tk2.ClaimedBy != nil {
		t.Fatalf("after abandon = %+v", tk2)
	}
	tk2, err = c2.ClaimTicket(ctx, tk2.ID.String(), tk2.CASVersion)
	if err != nil {
		t.Fatalf("re-claim after abandon: %v", err)
	}
	if tk2.Status != "claimed" || tk2.ClaimedBy == nil || tk2.ClaimedBy.ID != impl2.ID {
		t.Fatalf("after re-claim = %+v", tk2)
	}

	// Listing: newest first, status filter, cas_version included.
	open, err := c1.ListTickets(ctx, "claimed", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].ID != tk2.ID || open[0].CASVersion != tk2.CASVersion {
		t.Fatalf("list claimed = %+v", open)
	}
	all, err := c1.ListTickets(ctx, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].ID != tk2.ID {
		t.Fatalf("list all = %+v", all)
	}

	// GetTicket by slug carries the backing entry.
	got, err := c1.GetTicket(ctx, ticket.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if got.Entry == nil || !strings.Contains(got.Entry.Content, "It fails on Tuesdays.") {
		t.Fatalf("get ticket entry = %+v", got.Entry)
	}
}

// TestTicketScope: project tokens read global tickets but cannot
// mutate them.
func TestTicketScope(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	human := f.principal(t, "human", "scope-human")
	agent := f.principal(t, "agent", "scope-agent")
	proj := uuid.New()
	cAll := f.client(f.token(t, human.ID, nil))
	cP := f.client(f.token(t, agent.ID, &proj))

	global, err := cAll.CreateTicket(ctx, kbclient.CreateTicketRequest{
		Title: "Global chore", Body: "b",
	})
	if err != nil {
		t.Fatal(err)
	}
	if global.ProjectID != nil {
		t.Fatalf("scope-less default should be global, got %v", global.ProjectID)
	}

	// Visible to the project token…
	if _, err := cP.GetTicket(ctx, global.ID.String()); err != nil {
		t.Fatalf("project token reading global ticket: %v", err)
	}
	// …but not claimable by it.
	_, err = cP.ClaimTicket(ctx, global.ID.String(), 0)
	wantAPIError(t, err, http.StatusForbidden)
	_, err = cP.CommentTicket(ctx, global.ID.String(), "hi")
	wantAPIError(t, err, http.StatusForbidden)

	// Project tickets are invisible to other projects.
	mine, err := cP.CreateTicket(ctx, kbclient.CreateTicketRequest{Title: "Mine", Body: "b"})
	if err != nil {
		t.Fatal(err)
	}
	other := uuid.New()
	cOther := f.client(f.token(t, agent.ID, &other))
	_, err = cOther.GetTicket(ctx, mine.ID.String())
	wantAPIError(t, err, http.StatusNotFound)
}

// TestConcurrentClaim races N claimers through the real API on one
// ticket: exactly one wins, everyone else gets a 409 with current
// state, and the ticket ends with one claimant and cas_version 1.
func TestConcurrentClaim(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	proj := uuid.New()

	const claimers = 8
	clients := make([]*kbclient.Client, claimers)
	for i := range clients {
		p := f.principal(t, "agent", "racer")
		clients[i] = f.client(f.token(t, p.ID, &proj))
	}

	ticket, err := clients[0].CreateTicket(ctx, kbclient.CreateTicketRequest{
		Title: "Contended work", Body: "b",
	})
	if err != nil {
		t.Fatal(err)
	}

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		wins   int
		losses int
	)
	for _, c := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.ClaimTicket(ctx, ticket.ID.String(), 0)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				wins++
				return
			}
			var apiErr *kbclient.APIError
			if errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict && apiErr.Ticket != nil {
				losses++
				return
			}
			t.Errorf("unexpected claim error: %v", err)
		}()
	}
	wg.Wait()

	if wins != 1 || losses != claimers-1 {
		t.Fatalf("wins = %d, losses = %d (want 1 / %d)", wins, losses, claimers-1)
	}
	final, err := clients[0].GetTicket(ctx, ticket.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != "claimed" || final.CASVersion != 1 || final.ClaimedBy == nil {
		t.Fatalf("final state = %+v", final)
	}
}
