package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/bio4554/lab/internal/kbclient"
)

// Architecture-graph commands: kbase graph (render) and the kbase
// component group (add/link/unlink).

// cmdGraph renders the component graph in one of three formats. text
// is a readable adjacency list; dot and mermaid are valid Graphviz /
// Mermaid sources meant to be pasted into tooling and docs.
func cmdGraph(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("graph", flag.ContinueOnError)
	urlFlag, tokenFlag := connFlags(fs)
	format := fs.String("format", "text", "output format: text, dot or mermaid")
	atFlag := fs.String("at", "", "graph as of this RFC3339 time (default: now)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageError("usage: kbase graph [--format text|dot|mermaid] [--at <RFC3339>]")
	}
	var at time.Time
	if *atFlag != "" {
		at, err = time.Parse(time.RFC3339Nano, *atFlag)
		if err != nil {
			return usageError(fmt.Sprintf("invalid --at %q (want RFC3339)", *atFlag))
		}
	}
	c, err := client(*urlFlag, *tokenFlag)
	if err != nil {
		return err
	}
	g, err := c.Graph(ctx, at)
	if err != nil {
		return err
	}
	switch *format {
	case "text":
		renderText(stdout, g)
	case "dot":
		renderDot(stdout, g)
	case "mermaid":
		renderMermaid(stdout, g)
	default:
		return usageError(fmt.Sprintf("unknown format %q (want text, dot or mermaid)", *format))
	}
	return nil
}

// renderText writes an adjacency list: every node, with its outgoing
// edges indented beneath it.
func renderText(w io.Writer, g kbclient.Graph) {
	if len(g.Nodes) == 0 {
		fmt.Fprintln(w, "no components")
		return
	}
	for _, n := range g.Nodes {
		fmt.Fprintf(w, "%s (%s)\n", n.Slug, n.Title)
		for _, e := range g.Edges {
			if e.From == n.Slug {
				fmt.Fprintf(w, "  -> %s [%s]\n", e.To, e.Label)
			}
		}
	}
}

// dotQuote escapes a string for a double-quoted DOT id.
func dotQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// renderDot writes valid Graphviz source.
func renderDot(w io.Writer, g kbclient.Graph) {
	fmt.Fprintln(w, "digraph kbase {")
	for _, n := range g.Nodes {
		fmt.Fprintf(w, "  %s [label=%s];\n", dotQuote(n.Slug), dotQuote(n.Slug+" — "+n.Title))
	}
	for _, e := range g.Edges {
		fmt.Fprintf(w, "  %s -> %s [label=%s];\n",
			dotQuote(e.From), dotQuote(e.To), dotQuote(e.Label))
	}
	fmt.Fprintln(w, "}")
}

// mermaidID sanitizes a slug into a Mermaid-safe node id (dashes and
// anything exotic become underscores; the slug stays visible as the
// node text).
func mermaidID(slug string) string {
	var b strings.Builder
	for _, r := range slug {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return "n_" + b.String()
}

// renderMermaid writes a valid `graph LR` block (this gets pasted into
// docs — it must render).
func renderMermaid(w io.Writer, g kbclient.Graph) {
	fmt.Fprintln(w, "graph LR")
	for _, n := range g.Nodes {
		fmt.Fprintf(w, "  %s[\"%s\"]\n", mermaidID(n.Slug), strings.ReplaceAll(n.Slug, `"`, "'"))
	}
	for _, e := range g.Edges {
		label := strings.NewReplacer("|", "/", "\n", " ", `"`, "'").Replace(e.Label)
		fmt.Fprintf(w, "  %s -->|%s| %s\n", mermaidID(e.From), label, mermaidID(e.To))
	}
}

// cmdComponent dispatches the component subcommands.
func cmdComponent(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		return usageError("usage: kbase component add|link|unlink ...")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "add":
		return cmdComponentAdd(ctx, rest, stdin, stdout)
	case "link":
		return cmdComponentLink(ctx, rest, stdout)
	case "unlink":
		return cmdComponentUnlink(ctx, rest, stdout)
	default:
		return usageError(fmt.Sprintf("unknown component subcommand %q", sub))
	}
}

// cmdComponentAdd is sugar for `kbase add component`. Unlike add, the
// body is optional: without -m (or an explicit trailing "-" for
// stdin) the title doubles as the body, so registering a component is
// a one-liner.
func cmdComponentAdd(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("component add", flag.ContinueOnError)
	urlFlag, tokenFlag := connFlags(fs)
	title := fs.String("title", "", "component title (required)")
	slug := fs.String("slug", "", "explicit slug (default: derived from title)")
	global := fs.Bool("global", false, "write to the global scope (needs an all-projects token)")
	msg := fs.String("m", "", "description (default: the title)")
	wantStdin := slices.Contains(args, "-")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageError("usage: kbase component add --title <t> [--slug s] [--global] [-m body | -]")
	}
	if *title == "" {
		return usageError("--title is required")
	}
	body := *msg
	if wantStdin {
		body, err = content("", stdin)
		if err != nil {
			return err
		}
	}
	if body == "" {
		body = *title
	}
	c, err := client(*urlFlag, *tokenFlag)
	if err != nil {
		return err
	}
	entry, err := c.CreateEntry(ctx, kbclient.CreateEntryRequest{
		Type:    "component",
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

// cmdComponentLink creates a labeled directed edge between two
// components.
func cmdComponentLink(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("component link", flag.ContinueOnError)
	urlFlag, tokenFlag := connFlags(fs)
	label := fs.String("label", "", "edge label (required)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return usageError("usage: kbase component link <from> <to> --label <l>")
	}
	if *label == "" {
		return usageError("--label is required")
	}
	c, err := client(*urlFlag, *tokenFlag)
	if err != nil {
		return err
	}
	edge, err := c.CreateEdge(ctx, kbclient.CreateEdgeRequest{
		From: pos[0], To: pos[1], Label: *label,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "linked %s -> %s [%s] (edge %s)\n",
		edge.From, edge.To, edge.Label, edge.ID)
	return nil
}

// cmdComponentUnlink tombstones an edge, addressed by id or — when
// unambiguous — by <from> <to> [--label l].
func cmdComponentUnlink(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("component unlink", flag.ContinueOnError)
	urlFlag, tokenFlag := connFlags(fs)
	label := fs.String("label", "", "edge label (to disambiguate <from> <to>)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	c, err := client(*urlFlag, *tokenFlag)
	if err != nil {
		return err
	}
	var id uuid.UUID
	switch len(pos) {
	case 1:
		id, err = uuid.Parse(pos[0])
		if err != nil {
			return usageError(fmt.Sprintf("invalid edge id %q", pos[0]))
		}
	case 2:
		g, err := c.Graph(ctx, time.Time{})
		if err != nil {
			return err
		}
		var matches []kbclient.Edge
		for _, e := range g.Edges {
			if e.From == pos[0] && e.To == pos[1] && (*label == "" || e.Label == *label) {
				matches = append(matches, e)
			}
		}
		switch len(matches) {
		case 0:
			return fmt.Errorf("no live edge %s -> %s", pos[0], pos[1])
		case 1:
			id = matches[0].ID
		default:
			var ids []string
			for _, e := range matches {
				ids = append(ids, fmt.Sprintf("%s [%s]", e.ID, e.Label))
			}
			return fmt.Errorf("ambiguous: %d edges %s -> %s; unlink by id: %s",
				len(matches), pos[0], pos[1], strings.Join(ids, ", "))
		}
	default:
		return usageError("usage: kbase component unlink <edge-id> | <from> <to> [--label l]")
	}
	edge, err := c.TombstoneEdge(ctx, id)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "unlinked %s -> %s [%s]\n", edge.From, edge.To, edge.Label)
	return nil
}
