// Package tui drives the Bubble Tea-based migration UI.
//
// Flow:
//
//	scan → list (fuzzy filter + multi-select) → target path → confirm → apply
//
// The TUI groups sessions by (agent, cwd). Each group is the migration unit;
// selecting it moves every session that shared that cwd.
package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bengabay1994/mvs/internal/adapter"
	"github.com/bengabay1994/mvs/internal/backup"
	"github.com/bengabay1994/mvs/internal/session"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/sahilm/fuzzy"
)

// Group is one (agent, cwd) bucket displayed as a single TUI row.
type Group struct {
	Agent    string
	CWD      string
	Sessions []session.Session
	Latest   time.Time
	Bytes    int64
}

func (g Group) FilterValue() string { return g.Agent + " " + g.CWD }
func (g Group) Title() string       { return fmt.Sprintf("[%s] %s", g.Agent, g.CWD) }
func (g Group) Description() string {
	return fmt.Sprintf("%d session(s) · %s · last %s",
		len(g.Sessions), humanBytes(g.Bytes), humanRelTime(g.Latest))
}

type phase int

const (
	phaseScan phase = iota
	phaseList
	phaseTarget
	phaseConfirm
	phaseApplying
	phaseDone
)

type Model struct {
	phase phase

	groups   []Group
	visible  []int // indices into groups, current filter
	selected map[int]bool
	cursor   int

	filter textinput.Model
	target textinput.Model
	mode   session.Mode
	width  int
	height int

	plans   []session.Plan
	reports []session.Report
	runID   string
	err     error

	scanResult struct {
		groups []Group
		err    error
	}
}

// NewModel constructs a ready-to-run TUI model.
func NewModel(mode session.Mode) Model {
	fi := textinput.New()
	fi.Placeholder = "filter by path or agent…"
	fi.Prompt = "  "
	fi.CharLimit = 256
	fi.Width = 60
	fi.Focus()

	ti := textinput.New()
	ti.Placeholder = "/Users/you/path/to/new/project"
	ti.Prompt = "  "
	ti.CharLimit = 4096
	ti.Width = 80

	return Model{
		phase:    phaseScan,
		filter:   fi,
		target:   ti,
		selected: map[int]bool{},
		mode:     mode,
	}
}

// Init kicks off the discovery scan.
func (m Model) Init() tea.Cmd {
	return tea.Batch(scanCmd(), textinput.Blink)
}

type scanDoneMsg struct {
	groups []Group
	err    error
}

type applyDoneMsg struct {
	reports []session.Report
	err     error
	runID   string
}

func scanCmd() tea.Cmd {
	return func() tea.Msg {
		var all []session.Session
		for _, a := range adapter.All() {
			if !a.Available() {
				continue
			}
			s, err := a.Discover()
			if err != nil {
				return scanDoneMsg{err: err}
			}
			all = append(all, s...)
		}
		grouped := groupSessions(all)
		return scanDoneMsg{groups: grouped}
	}
}

func groupSessions(sessions []session.Session) []Group {
	bucket := map[string]*Group{}
	for _, s := range sessions {
		key := s.Agent + "\x00" + s.CWD
		g, ok := bucket[key]
		if !ok {
			g = &Group{Agent: s.Agent, CWD: s.CWD}
			bucket[key] = g
		}
		g.Sessions = append(g.Sessions, s)
		if s.Modified.After(g.Latest) {
			g.Latest = s.Modified
		}
		g.Bytes += s.Size
	}
	out := make([]Group, 0, len(bucket))
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

// Update is the Bubble Tea reducer.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case scanDoneMsg:
		m.groups = msg.groups
		m.scanResult.err = msg.err
		m.refilter()
		m.phase = phaseList
		return m, nil
	case applyDoneMsg:
		m.reports = msg.reports
		m.runID = msg.runID
		m.err = msg.err
		m.phase = phaseDone
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.phase {
	case phaseList:
		return m.handleListKey(msg)
	case phaseTarget:
		return m.handleTargetKey(msg)
	case phaseConfirm:
		return m.handleConfirmKey(msg)
	case phaseDone:
		switch msg.String() {
		case "q", "esc", "enter", "ctrl+c":
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m Model) handleListKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "q":
		// q only quits when filter has no text, otherwise treat as input
		if m.filter.Value() == "" || msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
	case "up", "ctrl+p":
		if m.cursor > 0 {
			m.cursor--
		}
		return m, nil
	case "down", "ctrl+n":
		if m.cursor < len(m.visible)-1 {
			m.cursor++
		}
		return m, nil
	case " ", "tab":
		if len(m.visible) == 0 {
			return m, nil
		}
		idx := m.visible[m.cursor]
		m.selected[idx] = !m.selected[idx]
		return m, nil
	case "enter":
		if m.anySelected() {
			m.phase = phaseTarget
			m.filter.Blur()
			m.target.Focus()
			return m, textinput.Blink
		}
		return m, nil
	case "ctrl+a":
		for _, i := range m.visible {
			m.selected[i] = true
		}
		return m, nil
	case "ctrl+x":
		m.selected = map[int]bool{}
		return m, nil
	}
	prev := m.filter.Value()
	newFilter, cmd := m.filter.Update(msg)
	m.filter = newFilter
	if m.filter.Value() != prev {
		m.refilter()
	}
	return m, cmd
}

func (m Model) handleTargetKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.phase = phaseList
		m.target.Blur()
		m.filter.Focus()
		return m, textinput.Blink
	case "enter":
		newCWD := strings.TrimSpace(m.target.Value())
		if newCWD == "" {
			return m, nil
		}
		// Build plans.
		plans, err := buildPlans(m, newCWD)
		if err != nil {
			m.err = err
			return m, nil
		}
		m.plans = plans
		m.phase = phaseConfirm
		return m, nil
	}
	newTarget, cmd := m.target.Update(msg)
	m.target = newTarget
	return m, cmd
}

func (m Model) handleConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "n", "esc":
		m.phase = phaseTarget
		return m, nil
	case "y", "enter":
		m.phase = phaseApplying
		runID := backup.NewRunID()
		plans := m.plans
		return m, func() tea.Msg {
			return runApply(runID, plans)
		}
	case "m":
		m.mode = session.ModeMove
		m.plans, _ = buildPlans(m, m.target.Value())
		return m, nil
	case "c":
		m.mode = session.ModeCopy
		m.plans, _ = buildPlans(m, m.target.Value())
		return m, nil
	}
	return m, nil
}

func runApply(runID string, plans []session.Plan) applyDoneMsg {
	bkDir, err := backup.Prepare(runID, plans)
	if err != nil {
		return applyDoneMsg{err: err, runID: runID}
	}
	var reports []session.Report
	for _, p := range plans {
		a, err := adapter.ByName(p.Agent)
		if err != nil {
			reports = append(reports, session.Report{Agent: p.Agent, OK: false, Errors: []string{err.Error()}})
			continue
		}
		r := a.Apply(p, session.ApplyOpts{BackupDir: bkDir})
		reports = append(reports, r)
	}
	_ = backup.Finalize(runID, reports)
	return applyDoneMsg{reports: reports, runID: runID}
}

func buildPlans(m Model, newCWD string) ([]session.Plan, error) {
	var plans []session.Plan
	for idx := range m.selected {
		if !m.selected[idx] {
			continue
		}
		g := m.groups[idx]
		a, err := adapter.ByName(g.Agent)
		if err != nil {
			return nil, err
		}
		p, err := a.Plan(g.Sessions, session.PlanOpts{NewCWD: newCWD, Mode: m.mode})
		if err != nil {
			return nil, err
		}
		plans = append(plans, p)
	}
	return plans, nil
}

func (m *Model) refilter() {
	q := strings.TrimSpace(m.filter.Value())
	m.visible = m.visible[:0]
	if q == "" {
		for i := range m.groups {
			m.visible = append(m.visible, i)
		}
	} else {
		corpus := make([]string, len(m.groups))
		for i, g := range m.groups {
			corpus[i] = g.FilterValue()
		}
		results := fuzzy.Find(q, corpus)
		for _, r := range results {
			m.visible = append(m.visible, r.Index)
		}
	}
	if m.cursor >= len(m.visible) {
		m.cursor = max0(len(m.visible) - 1)
	}
}

func (m Model) anySelected() bool {
	for _, v := range m.selected {
		if v {
			return true
		}
	}
	return false
}

// ---- view ----

var (
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7aa2f7"))
	dimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("#565f89"))
	accentStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#bb9af7"))
	okStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("#9ece6a"))
	errStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("#f7768e"))
	selectedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#e0af68")).Bold(true)
	cursorStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#7dcfff"))
	frame         = lipgloss.NewStyle().Padding(1, 2)
)

func (m Model) View() string {
	switch m.phase {
	case phaseScan:
		return frame.Render(titleStyle.Render("mvs") + "\n" + dimStyle.Render("scanning agents…"))
	case phaseList:
		return frame.Render(m.viewList())
	case phaseTarget:
		return frame.Render(m.viewTarget())
	case phaseConfirm:
		return frame.Render(m.viewConfirm())
	case phaseApplying:
		return frame.Render(titleStyle.Render("mvs") + "\n" + dimStyle.Render("applying…"))
	case phaseDone:
		return frame.Render(m.viewDone())
	}
	return ""
}

func (m Model) viewList() string {
	var b strings.Builder
	fmt.Fprintln(&b, titleStyle.Render("mvs · sessions"))
	fmt.Fprintln(&b, dimStyle.Render("filter:")+" "+m.filter.View())
	fmt.Fprintln(&b, "")
	if len(m.groups) == 0 {
		fmt.Fprintln(&b, dimStyle.Render("no sessions found on this host."))
		fmt.Fprintln(&b, dimStyle.Render("press q to quit."))
		return b.String()
	}
	maxRows := m.height - 9
	if maxRows < 5 {
		maxRows = 10
	}
	start := 0
	if m.cursor >= maxRows {
		start = m.cursor - maxRows + 1
	}
	end := start + maxRows
	if end > len(m.visible) {
		end = len(m.visible)
	}
	for vi := start; vi < end; vi++ {
		idx := m.visible[vi]
		g := m.groups[idx]
		marker := "[ ]"
		if m.selected[idx] {
			marker = selectedStyle.Render("[x]")
		}
		pointer := "  "
		line := fmt.Sprintf(" %s %s\n      %s",
			marker,
			g.Title(),
			dimStyle.Render(g.Description()))
		if vi == m.cursor {
			pointer = cursorStyle.Render("▸ ")
			line = cursorStyle.Render(line)
		}
		fmt.Fprint(&b, pointer)
		fmt.Fprintln(&b, line)
	}
	fmt.Fprintln(&b, "")
	fmt.Fprintln(&b, dimStyle.Render(
		"space: select  enter: continue  ctrl+a: select all  ctrl+x: clear  q: quit"))
	fmt.Fprintf(&b, "%s %s\n",
		dimStyle.Render("mode:"),
		accentStyle.Render(strings.ToUpper(m.mode.String())))
	return b.String()
}

func (m Model) viewTarget() string {
	var b strings.Builder
	fmt.Fprintln(&b, titleStyle.Render("mvs · target path"))
	fmt.Fprintln(&b, "")
	count := 0
	for _, v := range m.selected {
		if v {
			count++
		}
	}
	fmt.Fprintln(&b, dimStyle.Render(fmt.Sprintf("%d group(s) selected.", count)))
	fmt.Fprintln(&b, "")
	fmt.Fprintln(&b, "Enter the new cwd (absolute path):")
	fmt.Fprintln(&b, m.target.View())
	fmt.Fprintln(&b, "")
	fmt.Fprintln(&b, dimStyle.Render("enter: confirm  esc: back  ctrl+c: quit"))
	if m.err != nil {
		fmt.Fprintln(&b, errStyle.Render("error: "+m.err.Error()))
	}
	return b.String()
}

func (m Model) viewConfirm() string {
	var b strings.Builder
	fmt.Fprintln(&b, titleStyle.Render("mvs · confirm"))
	fmt.Fprintln(&b, "")
	fmt.Fprintf(&b, "mode: %s\n", accentStyle.Render(strings.ToUpper(m.mode.String())))
	fmt.Fprintln(&b, "")
	for _, p := range m.plans {
		fmt.Fprintf(&b, "%s  %s → %s\n",
			accentStyle.Render("["+p.Agent+"]"),
			p.OldCWD, p.NewCWD)
		for _, a := range p.Actions {
			fmt.Fprintf(&b, "    %s  %s %s\n",
				dimStyle.Render("·"),
				a.Kind,
				dimStyle.Render(a.Detail))
		}
	}
	fmt.Fprintln(&b, "")
	fmt.Fprintln(&b, dimStyle.Render("y: apply  n: back  m: switch to MOVE  c: switch to COPY  ctrl+c: quit"))
	return b.String()
}

func (m Model) viewDone() string {
	var b strings.Builder
	fmt.Fprintln(&b, titleStyle.Render("mvs · done"))
	fmt.Fprintln(&b, "")
	if m.err != nil {
		fmt.Fprintln(&b, errStyle.Render("error: "+m.err.Error()))
	}
	for _, r := range m.reports {
		status := okStyle.Render("✓")
		if !r.OK {
			status = errStyle.Render("✗")
		}
		fmt.Fprintf(&b, "%s [%s] %d action(s) applied\n", status, r.Agent, len(r.Applied))
		for _, e := range r.Errors {
			fmt.Fprintln(&b, "    "+errStyle.Render(e))
		}
	}
	fmt.Fprintln(&b, "")
	if m.runID != "" {
		fmt.Fprintf(&b, "%s mvs undo %s\n", dimStyle.Render("to roll back:"), accentStyle.Render(m.runID))
	}
	fmt.Fprintln(&b, "")
	fmt.Fprintln(&b, dimStyle.Render("press any key to quit."))
	return b.String()
}

// ---- formatting helpers ----

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func humanRelTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func max0(x int) int {
	if x < 0 {
		return 0
	}
	return x
}
