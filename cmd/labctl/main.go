// Command labctl is a throwaway dev CLI for driving the Phase 5 core
// loop directly against the internals (config → pgx pool → store /
// gitrepo / runtime / driver). There is no daemon API yet; Phase 7's
// TUI replaces this.
//
//	labctl project create -name X -origin <url|path> -stack go
//	labctl agent create -project X -name impl1 [-role "..."] [-model m] [-cred api_key|oauth_token]
//	labctl agent run -project X -name impl1
//	labctl send -project X -agent impl1 "prompt text"
//	labctl tail -project X -agent impl1
//	labctl retire -project X -agent impl1 [-reason "..."] -seed "..."
//	labctl merge -project X -agent impl1
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bio4554/lab/internal/config"
	"github.com/bio4554/lab/internal/labd/claude"
	"github.com/bio4554/lab/internal/labd/gitrepo"
	"github.com/bio4554/lab/internal/labd/runtime"
	"github.com/bio4554/lab/internal/labd/store"
	"github.com/bio4554/lab/internal/streamjson"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "labctl:", err)
		os.Exit(1)
	}
}

// app bundles the wired internals every subcommand shares.
type app struct {
	cfg config.Config
	st  *store.Store
	git *gitrepo.Manager
}

func run() error {
	args := os.Args[1:]
	// "agent create|run" is a two-word subcommand.
	if len(args) > 0 && (args[0] == "agent" || args[0] == "project") && len(args) > 1 {
		args = append([]string{args[0] + " " + args[1]}, args[2:]...)
	}
	if len(args) == 0 {
		return fmt.Errorf("usage: labctl <project create|agent create|agent run|send|tail|retire|merge> ...")
	}
	cmd, rest := args[0], args[1:]

	cfg, err := config.Load(config.DefaultPath, false)
	if err != nil {
		return err
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.SlogLevel()})))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, cfg.PostgresDSN)
	if err != nil {
		return err
	}
	defer pool.Close()
	a := app{cfg: cfg, st: store.New(pool), git: gitrepo.NewManager(cfg.DataDir)}

	switch cmd {
	case "project create":
		return a.projectCreate(ctx, rest)
	case "agent create":
		return a.agentCreate(ctx, rest)
	case "agent run":
		return a.agentRun(ctx, rest)
	case "send":
		return a.send(ctx, rest)
	case "tail":
		return a.tail(ctx, rest)
	case "retire":
		return a.retire(ctx, rest)
	case "merge":
		return a.merge(ctx, rest)
	default:
		return fmt.Errorf("unknown subcommand %q", cmd)
	}
}

func (a app) projectCreate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("project create", flag.ExitOnError)
	name := fs.String("name", "", "project name")
	origin := fs.String("origin", "", "git URL or local path")
	stack := fs.String("stack", "base", "stack: "+strings.Join(runtime.Stacks(), "|"))
	fs.Parse(args)
	if *name == "" || *origin == "" {
		return fmt.Errorf("project create: -name and -origin are required")
	}
	if !runtime.ValidStack(*stack) {
		return fmt.Errorf("project create: unknown stack %q (valid: %s)", *stack, strings.Join(runtime.Stacks(), ", "))
	}
	kind, origResolved := originKind(*origin)
	proj, err := a.st.CreateProject(ctx, store.NewProject{
		Name: *name, OriginKind: kind, Origin: origResolved, Stack: *stack,
	})
	if err != nil {
		return err
	}
	if err := a.git.CreateProject(ctx, proj.ID.String(), kind, origResolved); err != nil {
		a.st.DeleteProject(ctx, proj.ID) // don't leave a row without a repo
		return err
	}
	fmt.Printf("project %s created (id %s, %s %s, stack %s)\n", proj.Name, proj.ID, kind, origResolved, proj.Stack)
	return nil
}

// originKind classifies -origin as a git URL or a local path (made
// absolute so labd-side git operations don't depend on labctl's cwd).
func originKind(origin string) (kind, resolved string) {
	for _, p := range []string{"http://", "https://", "ssh://", "git://", "git@"} {
		if strings.HasPrefix(origin, p) {
			return store.OriginKindGitURL, origin
		}
	}
	if abs, err := filepath.Abs(origin); err == nil {
		return store.OriginKindLocalPath, abs
	}
	return store.OriginKindLocalPath, origin
}

func (a app) agentCreate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent create", flag.ExitOnError)
	project := fs.String("project", "", "project name")
	name := fs.String("name", "", "agent name")
	role := fs.String("role", "", "role prompt (--append-system-prompt)")
	model := fs.String("model", "", "model override")
	cred := fs.String("cred", "", "credential kind: api_key|oauth_token (secret read from env at run time)")
	fs.Parse(args)
	if *project == "" || *name == "" {
		return fmt.Errorf("agent create: -project and -name are required")
	}
	proj, err := a.st.GetProjectByName(ctx, *project)
	if err != nil {
		return err
	}
	na := store.NewAgent{
		ProjectID: proj.ID, Name: *name, RolePrompt: *role,
		Branch: gitrepo.BranchName(*name),
	}
	if *model != "" {
		na.Model = model
	}
	if *cred != "" {
		if *cred != store.CredentialKindAPIKey && *cred != store.CredentialKindOAuthToken {
			return fmt.Errorf("agent create: -cred must be api_key or oauth_token")
		}
		// Phase 5: the row records only the kind; the secret comes
		// from the driver's environment (see claude.EnvCredentialSource).
		c, err := a.st.CreateCredential(ctx, store.NewCredential{
			Kind: *cred, SecretEnc: []byte{}, Label: "env passthrough (" + *name + ")",
		})
		if err != nil {
			return err
		}
		na.CredentialID = &c.ID
	}
	agent, err := a.st.CreateAgent(ctx, na)
	if err != nil {
		return err
	}
	fmt.Printf("agent %s created (id %s, branch %s)\n", agent.Name, agent.ID, agent.Branch)
	return nil
}

// findAgent resolves -project/-agent names to the store rows.
func (a app) findAgent(ctx context.Context, project, name string) (store.Project, store.Agent, error) {
	proj, err := a.st.GetProjectByName(ctx, project)
	if err != nil {
		return store.Project{}, store.Agent{}, fmt.Errorf("project %q: %w", project, err)
	}
	agents, err := a.st.ListAgents(ctx, proj.ID)
	if err != nil {
		return store.Project{}, store.Agent{}, err
	}
	for _, ag := range agents {
		if ag.Name == name {
			return proj, ag, nil
		}
	}
	return store.Project{}, store.Agent{}, fmt.Errorf("agent %q not found in project %q", name, project)
}

func (a app) driver() (*claude.Driver, error) {
	rt, err := runtime.New()
	if err != nil {
		return nil, err
	}
	return claude.New(claude.Options{
		Store: a.st, Git: a.git, Runtime: rt,
		Builder: runtime.NewBuilder(rt, runtime.BuilderOptions{}),
	}), nil
}

func (a app) agentRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent run", flag.ExitOnError)
	project := fs.String("project", "", "project name")
	name := fs.String("name", "", "agent name")
	fs.Parse(args)
	_, agent, err := a.findAgent(ctx, *project, *name)
	if err != nil {
		return err
	}
	d, err := a.driver()
	if err != nil {
		return err
	}
	fmt.Printf("running agent %s (%s); Ctrl-C to stop\n", agent.Name, agent.ID)
	if err := d.Run(ctx, agent.ID); err != nil && ctx.Err() == nil {
		return err
	}
	fmt.Println("stopped")
	return nil
}

func (a app) send(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	project := fs.String("project", "", "project name")
	agentName := fs.String("agent", "", "agent name")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("send: exactly one prompt argument required")
	}
	_, agent, err := a.findAgent(ctx, *project, *agentName)
	if err != nil {
		return err
	}
	turn, err := a.st.EnqueueTurn(ctx, store.NewTurn{
		AgentID: agent.ID, SourceKind: store.SourceKindUser, Content: fs.Arg(0),
	})
	if err != nil {
		return err
	}
	fmt.Printf("queued turn %s\n", turn.ID)
	return nil
}

func (a app) tail(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("tail", flag.ExitOnError)
	project := fs.String("project", "", "project name")
	agentName := fs.String("agent", "", "agent name")
	fs.Parse(args)
	_, agent, err := a.findAgent(ctx, *project, *agentName)
	if err != nil {
		return err
	}
	var sessID = agent.ID // placeholder; replaced below
	var afterSeq int64
	first := true
	for ctx.Err() == nil {
		sess, err := a.st.CurrentSession(ctx, agent.ID)
		if err != nil {
			return err
		}
		if sess == nil {
			time.Sleep(time.Second)
			continue
		}
		if first || sess.ID != sessID {
			fmt.Printf("--- session %s (prev %v)\n", sess.ID, sess.PrevSessionID)
			sessID, afterSeq, first = sess.ID, 0, false
		}
		events, err := a.st.EventsSince(ctx, sess.ID, afterSeq, 200)
		if err != nil {
			return err
		}
		for _, e := range events {
			printEvent(e)
			afterSeq = e.Seq
		}
		if len(events) == 0 {
			time.Sleep(500 * time.Millisecond)
		}
	}
	return nil
}

// printEvent renders one persisted event via the streamjson accessors.
func printEvent(e store.Event) {
	ev := streamjson.Event{Raw: []byte(e.Payload)}
	var env struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
	}
	_ = json.Unmarshal(e.Payload, &env)
	ev.Kind, ev.Subtype = env.Type, env.Subtype

	label := e.Kind
	if ev.Subtype != "" {
		label += "/" + ev.Subtype
	}
	turn := ""
	if e.TurnID != nil {
		turn = " turn=" + e.TurnID.String()[:8]
	}
	var detail string
	switch e.Kind {
	case streamjson.KindAssistant, streamjson.KindUser:
		if text := ev.Text(); text != "" {
			detail = text
		}
		for _, tu := range ev.ToolUses() {
			in := string(tu.Input)
			if len(in) > 120 {
				in = in[:120] + "…"
			}
			detail += fmt.Sprintf("[tool_use %s %s]", tu.Name, in)
		}
	case streamjson.KindResult:
		if r, err := ev.Result(); err == nil {
			detail = fmt.Sprintf("is_error=%v turns=%d cost=$%.4f in=%d out=%d",
				r.IsError, r.NumTurns, r.TotalCostUSD, r.Usage.InputTokens, r.Usage.OutputTokens)
		}
	}
	if len(detail) > 500 {
		detail = detail[:500] + "…"
	}
	fmt.Printf("[%4d] %-18s%s %s\n", e.Seq, label, turn, strings.ReplaceAll(detail, "\n", "\n       "))
}

func (a app) retire(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("retire", flag.ExitOnError)
	project := fs.String("project", "", "project name")
	agentName := fs.String("agent", "", "agent name")
	reason := fs.String("reason", "manual retirement", "end reason recorded on the old session")
	seed := fs.String("seed", "", "first prompt of the new session")
	fs.Parse(args)
	_, agent, err := a.findAgent(ctx, *project, *agentName)
	if err != nil {
		return err
	}
	d, err := a.driver()
	if err != nil {
		return err
	}
	sess, err := d.Retire(ctx, agent.ID, *reason, *seed)
	if err != nil {
		return err
	}
	fmt.Printf("new session %s (prev %v)\n", sess.ID, sess.PrevSessionID)
	return nil
}

func (a app) merge(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("merge", flag.ExitOnError)
	project := fs.String("project", "", "project name")
	agentName := fs.String("agent", "", "agent name")
	fs.Parse(args)
	proj, agent, err := a.findAgent(ctx, *project, *agentName)
	if err != nil {
		return err
	}
	hash, err := a.git.Merge(ctx, proj.ID.String(), agent.Branch)
	if err != nil {
		return err
	}
	fmt.Printf("merged %s: %s\n", agent.Branch, hash)
	return nil
}
