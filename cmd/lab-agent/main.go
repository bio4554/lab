// Command lab-agent is the agent-facing control CLI: agents use it to
// see their siblings, drive each other with turns, report status, and
// (for orchestrators) spawn workers. It ships in every agent image
// alongside kbase and follows the same conventions.
//
// Connection comes from LAB_API_URL + LAB_AGENT_TOKEN (injected into
// agent containers by labd), overridable with -url/-token. Output is
// plain tabular text readable by humans and agents alike. Exit codes:
// 0 success, 1 API error, 2 usage.
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

	"github.com/bio4554/lab/internal/agentclient"
	"github.com/bio4554/lab/internal/wire"
)

// version is stamped via -ldflags at build time.
var version = "dev"

const usage = `lab-agent — agent control CLI for the lab daemon

Usage:
  lab-agent whoami
  lab-agent agents
  lab-agent send <agent> "<prompt>"     (or -m <prompt>, or prompt on stdin)
  lab-agent status "<text>"             ("" clears the status)
  lab-agent spawn --name <n> --role "<r>" [--role-file <f>] [--model <m>]
  lab-agent version

whoami  identifies you; agents lists your project's agents with state,
running flag, context occupancy and status. send queues a prompt for a
sibling agent (attributed to you). spawn creates and starts a worker
(orchestrators only; the worker inherits your credential).

Connection: LAB_API_URL + LAB_AGENT_TOKEN env vars; -url/-token override.
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
		fmt.Fprintf(stdout, "lab-agent %s\n", version)
		return 0
	}

	ctx := context.Background()
	var err error
	switch cmd {
	case "whoami":
		err = cmdWhoami(ctx, rest, stdout)
	case "agents":
		err = cmdAgents(ctx, rest, stdout)
	case "send":
		err = cmdSend(ctx, rest, stdin, stdout)
	case "status":
		err = cmdStatus(ctx, rest, stdout)
	case "spawn":
		err = cmdSpawn(ctx, rest, stdout)
	default:
		fmt.Fprintf(stderr, "lab-agent: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	var uerr usageError
	switch {
	case errors.As(err, &uerr):
		fmt.Fprintf(stderr, "lab-agent: %v\n", err)
		return 2
	case err != nil:
		fmt.Fprintf(stderr, "lab-agent: %v\n", err)
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
	urlFlag = fs.String("url", "", "agent API base URL (default $LAB_API_URL)")
	tokenFlag = fs.String("token", "", "bearer token (default $LAB_AGENT_TOKEN)")
	return
}

// client builds the API client from flags falling back to the env.
func client(urlFlag, tokenFlag string) (*agentclient.Client, error) {
	u := urlFlag
	if u == "" {
		u = os.Getenv("LAB_API_URL")
	}
	tok := tokenFlag
	if tok == "" {
		tok = os.Getenv("LAB_AGENT_TOKEN")
	}
	if u == "" {
		return nil, usageError("no agent API URL: set LAB_API_URL or pass -url")
	}
	if tok == "" {
		return nil, usageError("no token: set LAB_AGENT_TOKEN or pass -token")
	}
	return agentclient.New(u, tok), nil
}

// parse runs fs over args, allowing flags and positionals to be
// interspersed (stdlib flag would stop at the first positional). It
// returns the positionals with a trailing "-" (the explicit
// read-from-stdin marker) dropped; usage errors map to exit code 2.
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

func cmdWhoami(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("whoami", flag.ContinueOnError)
	urlFlag, tokenFlag := connFlags(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageError("usage: lab-agent whoami")
	}
	c, err := client(*urlFlag, *tokenFlag)
	if err != nil {
		return err
	}
	who, err := c.Whoami(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "name:    %s\n", who.Name)
	fmt.Fprintf(stdout, "agent:   %s\n", who.AgentID)
	fmt.Fprintf(stdout, "project: %s (%s)\n", who.Project, who.ProjectID)
	fmt.Fprintf(stdout, "state:   %s\n", who.State)
	if who.SessionID != nil {
		fmt.Fprintf(stdout, "session: %s\n", *who.SessionID)
	}
	if who.StatusText != nil {
		fmt.Fprintf(stdout, "status:  %s\n", *who.StatusText)
	}
	return nil
}

func cmdAgents(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("agents", flag.ContinueOnError)
	urlFlag, tokenFlag := connFlags(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageError("usage: lab-agent agents")
	}
	c, err := client(*urlFlag, *tokenFlag)
	if err != nil {
		return err
	}
	agents, err := c.Agents(ctx)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATE\tRUNNING\tCONTEXT\tSTATUS")
	for _, a := range agents {
		status := ""
		if a.StatusText != nil {
			status = *a.StatusText
		}
		fmt.Fprintf(tw, "%s\t%s\t%v\t%s\t%s\n",
			a.Name, a.State, a.Running, formatTokens(a.ContextTokens), status)
	}
	return tw.Flush()
}

// formatTokens renders a context-occupancy gauge value compactly:
// 512, 148k, 1.2M.
func formatTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%dk", n/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func cmdSend(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	urlFlag, tokenFlag := connFlags(fs)
	msg := fs.String("m", "", "prompt (default: second positional, or stdin)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 || len(pos) > 2 {
		return usageError(`usage: lab-agent send <agent> "<prompt>" (or -m, or stdin)`)
	}
	prompt := *msg
	if len(pos) == 2 {
		if prompt != "" {
			return usageError("pass the prompt as a positional or with -m, not both")
		}
		prompt = pos[1]
	}
	if prompt == "" {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return fmt.Errorf("reading stdin: %w", err)
		}
		prompt = strings.TrimSpace(string(b))
	}
	if prompt == "" {
		return usageError("empty prompt: pass it as a positional, with -m, or on stdin")
	}
	c, err := client(*urlFlag, *tokenFlag)
	if err != nil {
		return err
	}
	turn, err := c.SendTurn(ctx, pos[0], prompt)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "queued turn %s for %s\n", turn.ID, pos[0])
	return nil
}

func cmdStatus(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	urlFlag, tokenFlag := connFlags(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError(`usage: lab-agent status "<text>" ("" clears)`)
	}
	c, err := client(*urlFlag, *tokenFlag)
	if err != nil {
		return err
	}
	if err := c.ReportStatus(ctx, pos[0]); err != nil {
		return err
	}
	if pos[0] == "" {
		fmt.Fprintln(stdout, "status cleared")
	} else {
		fmt.Fprintln(stdout, "status reported")
	}
	return nil
}

func cmdSpawn(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("spawn", flag.ContinueOnError)
	urlFlag, tokenFlag := connFlags(fs)
	name := fs.String("name", "", "worker agent name (required)")
	role := fs.String("role", "", "role prompt")
	roleFile := fs.String("role-file", "", "read the role prompt from a file")
	model := fs.String("model", "", "model override")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageError("usage: lab-agent spawn --name <n> --role \"<r>\" [--role-file f] [--model m]")
	}
	if *name == "" {
		return usageError("--name is required")
	}
	if *role != "" && *roleFile != "" {
		return usageError("pass --role or --role-file, not both")
	}
	rolePrompt := *role
	if *roleFile != "" {
		b, err := os.ReadFile(*roleFile)
		if err != nil {
			return fmt.Errorf("reading role file: %w", err)
		}
		rolePrompt = string(b)
	}
	c, err := client(*urlFlag, *tokenFlag)
	if err != nil {
		return err
	}
	agent, err := c.Spawn(ctx, wire.SpawnAgentRequest{
		Name:       *name,
		RolePrompt: rolePrompt,
		Model:      *model,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "spawned agent %s (id %s, branch %s, running %v)\n",
		agent.Name, agent.ID, agent.Branch, agent.Running)
	return nil
}
