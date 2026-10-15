// mvs is a TUI for migrating agent session history between project directories.
//
// Subcommands:
//
//	mvs                    interactive TUI (default)
//	mvs list               print individual sessions (or --project groups)
//	mvs migrate FROM TO    select --session ID or --project explicitly
//	mvs undo RUN_ID        roll back a prior run from the backup snapshot
//	mvs doctor             print which agents are available and where
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bengabay1994/mvs/internal/adapter"
	"github.com/bengabay1994/mvs/internal/backup"
	"github.com/bengabay1994/mvs/internal/migration"
	"github.com/bengabay1994/mvs/internal/paths"
	"github.com/bengabay1994/mvs/internal/session"
	"github.com/bengabay1994/mvs/internal/tui"
	tea "github.com/charmbracelet/bubbletea"
)

const usage = `mvs — migrate agent session history between project directories

usage:
  mvs                     interactive session picker (tab: switch to projects)
  mvs list [--project]     show sessions with IDs, or whole project groups
  mvs migrate FROM TO     move selected sessions from FROM to TO
                          flags: --agent <claude|codex|gemini|opencode>
                                 --session <ID> select a session (repeatable)
                                 --project migrate every session at FROM
                                 --copy    duplicate instead of moving
                                 --dry-run plan only, no writes
  mvs undo RUN_ID         restore from a backup snapshot
  mvs doctor              show which adapters are available

global flags:
  --version               print version and exit
  --help                  this message
`

var version = "0.2.0"

func main() {
	if len(os.Args) < 2 {
		runTUI(session.ModeMove)
		return
	}
	switch os.Args[1] {
	case "-h", "--help", "help":
		fmt.Print(usage)
	case "--version":
		fmt.Println("mvs", version)
	case "list":
		runList(os.Args[2:])
	case "doctor":
		runDoctor()
	case "migrate":
		runMigrate(os.Args[2:])
	case "undo":
		runUndo(os.Args[2:])
	default:
		// Treat any other first arg as part of the default flow.
		fs := flag.NewFlagSet("mvs", flag.ExitOnError)
		copyMode := fs.Bool("copy", false, "duplicate instead of moving")
		_ = fs.Parse(os.Args[1:])
		mode := session.ModeMove
		if *copyMode {
			mode = session.ModeCopy
		}
		runTUI(mode)
	}
}

func runTUI(mode session.Mode) {
	p := tea.NewProgram(tui.NewModel(mode), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "tui error:", err)
		os.Exit(1)
	}
}

func runList(args []string) {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	projects := fs.Bool("project", false, "group all sessions by agent and project")
	_ = fs.Parse(args)
	groups := scanGroups()
	if len(groups) == 0 {
		fmt.Println("(no sessions found)")
		return
	}
	for _, g := range groups {
		if *projects {
			fmt.Printf("[%s] %s  (%d sessions)\n", g.Agent, g.CWD, len(g.Sessions))
			continue
		}
		for _, s := range g.Sessions {
			if !s.ProjectOnly {
				fmt.Printf("[%s] %s  %s  %s\n", s.Agent, s.ID, s.CWD, strings.Join(strings.Fields(s.Title), " "))
			}
		}
	}
}

func runDoctor() {
	fmt.Println("home:", paths.Home())
	for _, a := range adapter.All() {
		var statusRoot string
		switch a.Name() {
		case "claude":
			statusRoot = paths.ClaudeRoot()
		case "codex":
			statusRoot = paths.CodexRoot()
		case "gemini":
			statusRoot = paths.GeminiRoot()
		case "opencode":
			statusRoot = paths.OpenCodeRoot()
		}
		status := "missing"
		if a.Available() {
			status = "ok"
		}
		fmt.Printf("  %-9s %-7s %s\n", a.Name(), status, statusRoot)
	}
	fmt.Println("backups:", filepath.Join(paths.MvsStateRoot(), "backups"))
}

func runMigrate(args []string) {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	agentFlag := fs.String("agent", "", "limit to a single agent (claude|codex|gemini|opencode)")
	copyMode := fs.Bool("copy", false, "duplicate instead of moving")
	dryRun := fs.Bool("dry-run", false, "print the plan; don't write")
	projects := fs.Bool("project", false, "migrate every session in each matching project")
	var ids sessionFlags
	fs.Var(&ids, "session", "session ID to migrate (repeatable; see mvs list)")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	rest := fs.Args()
	if len(rest) != 2 {
		fmt.Fprintln(os.Stderr, "usage: mvs migrate [flags] FROM TO")
		os.Exit(2)
	}
	from := paths.NormalizeCWD(rest[0])
	to := paths.NormalizeCWD(rest[1])
	if from == "" || to == "" {
		fmt.Fprintln(os.Stderr, "FROM and TO must be non-empty paths")
		os.Exit(2)
	}
	mode := session.ModeMove
	if *copyMode {
		mode = session.ModeCopy
	}

	planned, err := planMigration(scanGroups(), from, to, *agentFlag, ids, *projects, mode)
	if err != nil {
		fmt.Fprintln(os.Stderr, "plan:", err)
		os.Exit(1)
	}
	printPlans(planned)
	if *dryRun {
		fmt.Println("(dry run; no writes)")
		return
	}

	runID, reports, err := migration.Execute(planned)
	printReports(reports)
	if err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		if runID != "" {
			fmt.Fprintln(os.Stderr, "backup id:", runID)
		}
		os.Exit(1)
	}
	fmt.Println("backup id:", runID)
	fmt.Println("undo with: mvs undo", runID)
}

type sessionFlags []string

func (s *sessionFlags) String() string { return strings.Join(*s, ", ") }
func (s *sessionFlags) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("session ID must not be empty")
	}
	*s = append(*s, value)
	return nil
}

func planMigration(groups []tui.Group, from, to, agent string, ids []string, projects bool, mode session.Mode) ([]session.Plan, error) {
	if projects && len(ids) > 0 {
		return nil, fmt.Errorf("--project and --session cannot be combined")
	}
	if !projects && len(ids) == 0 {
		return nil, fmt.Errorf("select --session ID (see mvs list), or --project to migrate all sessions")
	}
	requested := make(map[string]int, len(ids))
	for _, id := range ids {
		requested[id] = 0
	}
	scope := session.ScopeSession
	if projects {
		scope = session.ScopeProject
	}
	var planned []session.Plan
	for _, g := range groups {
		if g.CWD != from || (agent != "" && g.Agent != agent) {
			continue
		}
		var selected []session.Session
		for _, s := range g.Sessions {
			if projects {
				selected = append(selected, s)
				continue
			}
			if _, ok := requested[s.ID]; ok && !s.ProjectOnly {
				selected = append(selected, s)
				requested[s.ID]++
			}
		}
		if len(selected) == 0 {
			continue
		}
		a, err := adapter.ByName(g.Agent)
		if err != nil {
			return nil, err
		}
		p, err := a.Plan(selected, session.PlanOpts{NewCWD: to, Mode: mode, Scope: scope})
		if err != nil {
			return nil, err
		}
		planned = append(planned, p)
	}
	for id, count := range requested {
		if count == 0 {
			return nil, fmt.Errorf("session %q was not found at %s for the selected agent", id, from)
		}
		if count > 1 {
			return nil, fmt.Errorf("session ID %q is ambiguous; use --agent to select its agent", id)
		}
	}
	if len(planned) == 0 {
		return nil, fmt.Errorf("no sessions match %s", from)
	}
	if err := migration.Validate(planned); err != nil {
		return nil, err
	}
	return planned, nil
}

func runUndo(args []string) {
	if len(args) == 0 {
		manifests, _ := backup.ListRuns()
		if len(manifests) == 0 {
			fmt.Println("(no backups)")
			return
		}
		fmt.Println("known runs (newest first):")
		for _, m := range manifests {
			fmt.Printf("  %s  %d plan(s)  %s\n",
				m.ID, len(m.Plans), m.Timestamp.Local().Format("2006-01-02 15:04"))
		}
		return
	}
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: mvs undo RUN_ID")
		os.Exit(2)
	}
	id := args[0]
	if err := backup.Undo(id); err != nil {
		fmt.Fprintln(os.Stderr, "undo:", err)
		os.Exit(1)
	}
	fmt.Println("restored backup", id)
}

func scanGroups() []tui.Group {
	var all []session.Session
	for _, a := range adapter.All() {
		if !a.Available() {
			continue
		}
		s, err := a.Discover()
		if err != nil {
			fmt.Fprintln(os.Stderr, "discover", a.Name()+":", err)
			continue
		}
		all = append(all, s...)
	}
	// Group inline to avoid exporting tui.groupSessions.
	bucket := map[string]*tui.Group{}
	for _, s := range all {
		key := s.Agent + "\x00" + s.CWD
		g, ok := bucket[key]
		if !ok {
			g = &tui.Group{Agent: s.Agent, CWD: s.CWD}
			bucket[key] = g
		}
		g.Sessions = append(g.Sessions, s)
		if s.Modified.After(g.Latest) {
			g.Latest = s.Modified
		}
		g.Bytes += s.Size
	}
	out := make([]tui.Group, 0, len(bucket))
	for _, g := range bucket {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Latest.Equal(out[j].Latest) {
			return out[i].Latest.After(out[j].Latest)
		}
		return out[i].CWD < out[j].CWD
	})
	return out
}

func printPlans(plans []session.Plan) {
	for _, p := range plans {
		fmt.Printf("[%s] %s → %s (%s)\n", p.Agent, p.OldCWD, p.NewCWD, p.Mode)
		if p.Scope == session.ScopeSession.String() {
			fmt.Println("    selected sessions:", strings.Join(p.SessionIDs, ", "))
		}
		for _, a := range p.Actions {
			detail := a.Detail
			if detail != "" {
				detail = "  " + detail
			}
			fmt.Printf("    · %s%s\n", a.Kind, detail)
		}
	}
}

func printReports(reports []session.Report) {
	for _, r := range reports {
		status := "ok"
		if !r.OK {
			status = "FAIL"
		}
		fmt.Printf("[%s] %s  (%d action(s))\n", r.Agent, status, len(r.Applied))
		for _, e := range r.Errors {
			fmt.Println("    !", strings.TrimSpace(e))
		}
	}
}
