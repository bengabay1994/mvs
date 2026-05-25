// mvs is a TUI for migrating agent session history between project directories.
//
// Subcommands:
//
//	mvs                    interactive TUI (default)
//	mvs list               print every (agent, cwd) group found on this host
//	mvs migrate FROM TO    non-interactive migration (use --agent to scope)
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
	"github.com/bengabay1994/mvs/internal/paths"
	"github.com/bengabay1994/mvs/internal/session"
	"github.com/bengabay1994/mvs/internal/tui"
	tea "github.com/charmbracelet/bubbletea"
)

const usage = `mvs — migrate agent session history between project directories

usage:
  mvs                     interactive TUI (default)
  mvs list                show every (agent, cwd) group
  mvs migrate FROM TO     migrate sessions whose old cwd = FROM to TO
                          flags: --agent <claude|codex|gemini|opencode>
                                 --copy    duplicate instead of moving
                                 --dry-run plan only, no writes
  mvs undo RUN_ID         restore from a backup snapshot
  mvs doctor              show which adapters are available

global flags:
  --version               print version and exit
  --help                  this message
`

var version = "0.1.0"

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
		runList()
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

func runList() {
	groups := scanGroups()
	if len(groups) == 0 {
		fmt.Println("(no sessions found)")
		return
	}
	for _, g := range groups {
		fmt.Printf("[%s] %s  (%d sessions)\n", g.Agent, g.CWD, len(g.Sessions))
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

	groups := scanGroups()
	var planned []session.Plan
	for _, g := range groups {
		if g.CWD != from {
			continue
		}
		if *agentFlag != "" && g.Agent != *agentFlag {
			continue
		}
		a, err := adapter.ByName(g.Agent)
		if err != nil {
			continue
		}
		p, err := a.Plan(g.Sessions, session.PlanOpts{NewCWD: to, Mode: mode})
		if err != nil {
			fmt.Fprintln(os.Stderr, "plan:", err)
			os.Exit(1)
		}
		planned = append(planned, p)
	}
	if len(planned) == 0 {
		fmt.Fprintln(os.Stderr, "no sessions match", from)
		os.Exit(1)
	}
	printPlans(planned)
	if *dryRun {
		fmt.Println("(dry run; no writes)")
		return
	}

	runID := backup.NewRunID()
	bkDir, err := backup.Prepare(runID, planned)
	if err != nil {
		fmt.Fprintln(os.Stderr, "prepare backup:", err)
		os.Exit(1)
	}
	var reports []session.Report
	for _, p := range planned {
		a, _ := adapter.ByName(p.Agent)
		r := a.Apply(p, session.ApplyOpts{BackupDir: bkDir})
		reports = append(reports, r)
	}
	_ = backup.Finalize(runID, reports)
	printReports(reports)
	fmt.Println("backup id:", runID)
	fmt.Println("undo with: mvs undo", runID)
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
