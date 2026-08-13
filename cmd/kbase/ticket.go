package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/bio4554/lab/internal/kbclient"
)

// Ticket commands. Tickets are claimed with a single compare-and-swap
// attempt — no internal retry — so scripted concurrent claimers behave
// predictably: exactly one exits 0, the rest print the current state
// and exit 1.

// cmdTicket dispatches the ticket subcommands.
func cmdTicket(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return usageError("usage: kbase ticket list|show|claim|start|done|abandon|comment ...")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return cmdTicketList(ctx, rest, stdout)
	case "show":
		return cmdTicketShow(ctx, rest, stdout)
	case "claim", "start", "done", "abandon":
		return cmdTicketTransition(ctx, sub, rest, stdout)
	case "comment":
		return cmdTicketComment(ctx, rest, stdout)
	default:
		return usageError(fmt.Sprintf("unknown ticket subcommand %q", sub))
	}
}

func cmdTicketList(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("ticket list", flag.ContinueOnError)
	urlFlag, tokenFlag := connFlags(fs)
	status := fs.String("status", "", "filter by status (open, claimed, in_progress, done, abandoned)")
	limit := fs.Int("limit", 0, "max tickets (default: server default)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageError("usage: kbase ticket list [--status s] [--limit n]")
	}
	c, err := client(*urlFlag, *tokenFlag)
	if err != nil {
		return err
	}
	tickets, err := c.ListTickets(ctx, *status, *limit)
	if err != nil {
		return err
	}
	if len(tickets) == 0 {
		fmt.Fprintln(stdout, "no tickets")
		return nil
	}
	tw := tabwriter.NewWriter(stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SLUG\tSTATUS\tCAS\tTITLE\tCLAIMED-BY\tUPDATED")
	for _, t := range tickets {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\n",
			t.Slug, t.Status, t.CASVersion, t.Title,
			claimantLabel(t), t.UpdatedAt.Local().Format(timeFmt))
	}
	return tw.Flush()
}

func cmdTicketShow(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("ticket show", flag.ContinueOnError)
	urlFlag, tokenFlag := connFlags(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError("usage: kbase ticket show <id|slug>")
	}
	c, err := client(*urlFlag, *tokenFlag)
	if err != nil {
		return err
	}
	t, err := c.GetTicket(ctx, pos[0])
	if err != nil {
		return err
	}
	printTicket(stdout, t)
	if t.Entry != nil {
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, t.Entry.Content)
	}
	return nil
}

// printTicket writes the one-look summary of a ticket's coordination
// state.
func printTicket(w io.Writer, t kbclient.Ticket) {
	fmt.Fprintf(w, "# %s\n\n", t.Title)
	fmt.Fprintf(w, "ticket: %s | slug: %s | status: %s | cas: %d\n",
		t.ID, t.Slug, t.Status, t.CASVersion)
	fmt.Fprintf(w, "claimed by: %s | updated: %s\n",
		claimantLabel(t), t.UpdatedAt.Local().Format(timeFmt))
}

func claimantLabel(t kbclient.Ticket) string {
	if t.ClaimedBy == nil {
		return "-"
	}
	return authorLabel(*t.ClaimedBy)
}

// cmdTicketTransition runs claim/start/done/abandon: one GET for the
// current cas_version, then exactly one CAS attempt. A 409 prints the
// ticket's current state (from the error body) and exits 1 — callers
// re-list and pick other work; the CLI never retries for them.
func cmdTicketTransition(ctx context.Context, action string, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("ticket "+action, flag.ContinueOnError)
	urlFlag, tokenFlag := connFlags(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError(fmt.Sprintf("usage: kbase ticket %s <id|slug>", action))
	}
	c, err := client(*urlFlag, *tokenFlag)
	if err != nil {
		return err
	}
	t, err := c.GetTicket(ctx, pos[0])
	if err != nil {
		return err
	}
	var do func(context.Context, string, int) (kbclient.Ticket, error)
	switch action {
	case "claim":
		do = c.ClaimTicket
	case "start":
		do = c.StartTicket
	case "done":
		do = c.DoneTicket
	case "abandon":
		do = c.AbandonTicket
	}
	updated, err := do(ctx, t.ID.String(), t.CASVersion)
	var apiErr *kbclient.APIError
	if errors.As(err, &apiErr) && apiErr.Ticket != nil {
		fmt.Fprintf(stdout, "%s failed — current state:\n", action)
		printTicket(stdout, *apiErr.Ticket)
		return err
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s: %s is now %s (cas %d)\n",
		action, updated.Slug, updated.Status, updated.CASVersion)
	return nil
}

func cmdTicketComment(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("ticket comment", flag.ContinueOnError)
	urlFlag, tokenFlag := connFlags(fs)
	msg := fs.String("m", "", "comment text (required)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError(`usage: kbase ticket comment <id|slug> -m "text"`)
	}
	if *msg == "" {
		return usageError("-m is required")
	}
	c, err := client(*urlFlag, *tokenFlag)
	if err != nil {
		return err
	}
	t, err := c.CommentTicket(ctx, pos[0], *msg)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "commented on %s\n", t.Slug)
	return nil
}
