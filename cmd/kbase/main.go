// Command kbase is the agent-facing knowledge-base CLI. It ships in
// every agent image; agents (and humans) use it to record decisions,
// notes and architecture and to recall them by full-text search.
//
// Connection comes from KBASE_URL + KBASE_TOKEN (injected into agent
// containers by labd), overridable with -url/-token. Output is plain
// tabular/markdown text readable by humans and agents alike. Exit
// codes: 0 success, 1 error, 2 usage.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/bio4554/lab/internal/kbclient"
)

// version is stamped via -ldflags at build time.
var version = "dev"

const usage = `kbase — versioned, append-only knowledge base

Usage:
  kbase add <type> --title <t> [--slug <s>] [--global] [-m <content>] [-]
  kbase update <slug> [--title <t>] [-m <content>] [-]
  kbase show <slug> [--version <n>] [--history]
  kbase list [--type <t>] [--limit <n>]
  kbase recall "<query>" [--type <t>] [-n <count>]
  kbase graph [--format text|dot|mermaid] [--at <RFC3339>]
  kbase component add --title <t> [--slug <s>] [-m <body> | -]
  kbase component link <from> <to> --label <l>
  kbase component unlink <edge-id> | <from> <to> [--label <l>]
  kbase ticket list [--status <s>] [--limit <n>]
  kbase ticket show <id|slug>
  kbase ticket claim|start|done|abandon <id|slug>
  kbase ticket comment <id|slug> -m "<text>"
  kbase version

Types: decision, note, architecture, component, ticket.
Content comes from -m or stdin (a trailing "-" forces stdin).
"add ticket" creates a real ticket (entry + claimable ticket row).

Connection: KBASE_URL + KBASE_TOKEN env vars; -url/-token override.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run executes one CLI invocation; factored out of main for tests.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	case "version", "-version", "--version":
		fmt.Fprintf(stdout, "kbase %s\n", version)
		return 0
	}

	ctx := context.Background()
	var err error
	switch cmd {
	case "add":
		err = cmdAdd(ctx, rest, stdin, stdout)
	case "update":
		err = cmdUpdate(ctx, rest, stdin, stdout)
	case "show":
		err = cmdShow(ctx, rest, stdout)
	case "list":
		err = cmdList(ctx, rest, stdout)
	case "recall":
		err = cmdRecall(ctx, rest, stdout)
	case "graph":
		err = cmdGraph(ctx, rest, stdout)
	case "component":
		err = cmdComponent(ctx, rest, stdin, stdout)
	case "ticket":
		err = cmdTicket(ctx, rest, stdout)
	default:
		fmt.Fprintf(stderr, "kbase: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	var uerr usageError
	switch {
	case errors.As(err, &uerr):
		fmt.Fprintf(stderr, "kbase: %v\n", err)
		return 2
	case err != nil:
		fmt.Fprintf(stderr, "kbase: %v\n", err)
		return 1
	}
	return 0
}

// usageError marks bad invocations (exit 2) as opposed to failed ones
// (exit 1).
type usageError string

func (e usageError) Error() string { return string(e) }

// connFlags adds the -url/-token overrides to fs.
func connFlags(fs *flag.FlagSet) (urlFlag, tokenFlag *string) {
	urlFlag = fs.String("url", "", "kbased base URL (default $KBASE_URL)")
	tokenFlag = fs.String("token", "", "bearer token (default $KBASE_TOKEN)")
	return
}

// client builds the API client from flags falling back to the env.
func client(urlFlag, tokenFlag string) (*kbclient.Client, error) {
	u := urlFlag
	if u == "" {
		u = os.Getenv("KBASE_URL")
	}
	tok := tokenFlag
	if tok == "" {
		tok = os.Getenv("KBASE_TOKEN")
	}
	if u == "" {
		return nil, usageError("no kbased URL: set KBASE_URL or pass -url")
	}
	if tok == "" {
		return nil, usageError("no token: set KBASE_TOKEN or pass -token")
	}
	return kbclient.New(u, tok), nil
}

// parse runs fs over args, allowing flags and positionals to be
// interspersed (`kbase add decision --title t`; stdlib flag would stop
// at the first positional). It returns the positionals with a trailing
// "-" (the explicit read-from-stdin marker) dropped; usage errors map
// to exit code 2.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	for len(args) > 0 {
		for len(args) > 0 && (args[0] == "-" || !strings.HasPrefix(args[0], "-")) {
			pos = append(pos, args[0])
			args = args[1:]
		}
		if len(args) == 0 {
			break
		}
		if err := fs.Parse(args); err != nil {
			return nil, usageError(err.Error())
		}
		args = fs.Args()
	}
	if n := len(pos); n > 0 && pos[n-1] == "-" {
		pos = pos[:n-1]
	}
	return pos, nil
}

// content returns the entry body: -m if given, otherwise stdin. A
// bare "-" positional also means stdin (and is how you force it).
func content(m string, stdin io.Reader) (string, error) {
	if m != "" {
		return m, nil
	}
	b, err := io.ReadAll(stdin)
	if err != nil {
		return "", fmt.Errorf("reading stdin: %w", err)
	}
	text := strings.TrimSpace(string(b))
	if text == "" {
		return "", usageError("empty content: pass -m or pipe the body on stdin")
	}
	return text, nil
}

func cmdAdd(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	urlFlag, tokenFlag := connFlags(fs)
	title := fs.String("title", "", "entry title (required)")
	slug := fs.String("slug", "", "explicit slug (default: derived from title)")
	global := fs.Bool("global", false, "write to the global scope (needs an all-projects token)")
	msg := fs.String("m", "", "content (default: read stdin)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError("usage: kbase add <type> --title <t> [--slug s] [--global] [-m content] [-]")
	}
	if *title == "" {
		return usageError("--title is required")
	}
	body, err := content(*msg, stdin)
	if err != nil {
		return err
	}
	c, err := client(*urlFlag, *tokenFlag)
	if err != nil {
		return err
	}
	// Ticket entries are 1:1 with a claimable ticket row, so "add
	// ticket" goes through the ticket API (which creates both in one
	// transaction); kbased rejects bare ticket entries.
	if pos[0] == "ticket" {
		if *global {
			return usageError("tickets have no --global flag (scope-less tokens default to global)")
		}
		ticket, err := c.CreateTicket(ctx, kbclient.CreateTicketRequest{
			Title: *title,
			Body:  body,
			Slug:  *slug,
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "created ticket %s (%s, cas %d)\n",
			ticket.Slug, ticket.Status, ticket.CASVersion)
		return nil
	}
	entry, err := c.CreateEntry(ctx, kbclient.CreateEntryRequest{
		Type:    pos[0],
		Slug:    *slug,
		Title:   *title,
		Content: body,
		Global:  *global,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "created %s v%d (%s, %s)\n",
		entry.Slug, entry.VersionNo, entry.Type, scopeLabel(entry))
	return nil
}

func cmdUpdate(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	urlFlag, tokenFlag := connFlags(fs)
	title := fs.String("title", "", "new title (default: keep the current one)")
	msg := fs.String("m", "", "content (default: read stdin)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError("usage: kbase update <slug> [--title t] [-m content] [-]")
	}
	body, err := content(*msg, stdin)
	if err != nil {
		return err
	}
	c, err := client(*urlFlag, *tokenFlag)
	if err != nil {
		return err
	}
	entry, err := c.AppendVersion(ctx, pos[0], kbclient.AppendVersionRequest{
		Title:   *title,
		Content: body,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "updated %s to v%d\n", entry.Slug, entry.VersionNo)
	return nil
}

func cmdShow(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	urlFlag, tokenFlag := connFlags(fs)
	versionNo := fs.Int("version", 0, "show a specific version (default: current)")
	history := fs.Bool("history", false, "include the version history")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError("usage: kbase show <slug> [--version n] [--history]")
	}
	c, err := client(*urlFlag, *tokenFlag)
	if err != nil {
		return err
	}
	entry, err := c.GetEntry(ctx, pos[0], kbclient.GetEntryOptions{
		Version: *versionNo,
		History: *history,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "# %s\n\n", entry.Title)
	fmt.Fprintf(stdout, "slug: %s | type: %s | version: %d | scope: %s\n",
		entry.Slug, entry.Type, entry.VersionNo, scopeLabel(entry))
	fmt.Fprintf(stdout, "author: %s | updated: %s\n\n",
		authorLabel(entry.Author), entry.UpdatedAt.Local().Format(timeFmt))
	fmt.Fprintln(stdout, entry.Content)
	if *history {
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "History:")
		tw := tabwriter.NewWriter(stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "  VERSION\tTITLE\tAUTHOR\tCREATED")
		for _, v := range entry.History {
			fmt.Fprintf(tw, "  v%d\t%s\t%s\t%s\n",
				v.VersionNo, v.Title, authorLabel(v.Author), v.CreatedAt.Local().Format(timeFmt))
		}
		tw.Flush()
	}
	return nil
}

func cmdList(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	urlFlag, tokenFlag := connFlags(fs)
	typ := fs.String("type", "", "filter by entry type")
	limit := fs.Int("limit", 0, "max entries (default: server default)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageError("usage: kbase list [--type t] [--limit n]")
	}
	c, err := client(*urlFlag, *tokenFlag)
	if err != nil {
		return err
	}
	entries, err := c.ListEntries(ctx, *typ, *limit)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Fprintln(stdout, "no entries")
		return nil
	}
	tw := tabwriter.NewWriter(stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SLUG\tTYPE\tVER\tTITLE\tAUTHOR\tUPDATED")
	for _, e := range entries {
		fmt.Fprintf(tw, "%s\t%s\tv%d\t%s\t%s\t%s\n",
			e.Slug, e.Type, e.VersionNo, e.Title,
			authorLabel(e.Author), e.UpdatedAt.Local().Format(timeFmt))
	}
	return tw.Flush()
}

func cmdRecall(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("recall", flag.ContinueOnError)
	urlFlag, tokenFlag := connFlags(fs)
	typ := fs.String("type", "", "filter by entry type")
	n := fs.Int("n", 0, "max results (default 8)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError(`usage: kbase recall "<query>" [--type t] [-n 8]`)
	}
	c, err := client(*urlFlag, *tokenFlag)
	if err != nil {
		return err
	}
	hits, err := c.Recall(ctx, pos[0], *typ, *n)
	if err != nil {
		return err
	}
	if len(hits) == 0 {
		fmt.Fprintln(stdout, "no matches")
		return nil
	}
	for i, h := range hits {
		fmt.Fprintf(stdout, "%d. %s (%s) — %s\n", i+1, h.Slug, h.Type, h.Title)
		if h.Excerpt != "" {
			fmt.Fprintf(stdout, "   %s\n", h.Excerpt)
		}
	}
	return nil
}

const timeFmt = "2006-01-02 15:04"

func scopeLabel(e kbclient.Entry) string {
	if e.ProjectID == nil {
		return "global"
	}
	return "project " + e.ProjectID.String()
}

func authorLabel(a kbclient.Author) string {
	name := a.DisplayName
	if name == "" {
		name = a.ExternalID
	}
	return a.Kind + ":" + name
}
