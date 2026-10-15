// Package tui drives the Bubble Tea-based migration UI.
//
// Flow:
//
//	scan → list (fuzzy filter + multi-select) → target path → confirm → apply
//
// Individual sessions are the default selection unit. Tab switches to projects,
// where each row contains every session sharing an agent and cwd.
package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/bengabay1994/mvs/internal/adapter"
	"github.com/bengabay1994/mvs/internal/migration"
	"github.com/bengabay1994/mvs/internal/paths"
	"github.com/bengabay1994/mvs/internal/session"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/sahilm/fuzzy"
)

// Group is a selectable row: one session, or a whole (agent, cwd) project.
type Group struct {
	Agent    string
	CWD      string
	Sessions []session.Session
	Latest   time.Time
	Bytes    int64
	Scope    session.Scope
}

func (g Group) FilterValue() string {
	var value strings.Builder
	value.WriteString(g.Agent + " " + g.CWD)
	for _, s := range g.Sessions {
		value.WriteString(" " + s.Title + " " + s.ID)
	}
	return value.String()
}
func (g Group) Title() string {
	if g.Scope == session.ScopeSession && len(g.Sessions) == 1 {
		s := g.Sessions[0]
		title := displayText(s.Title)
		if title == "" {
			title = s.ID
		}
		return fmt.Sprintf("[%s] %s", g.Agent, title)
	}
	return fmt.Sprintf("[%s] %s", g.Agent, g.CWD)
}
func (g Group) Description() string {
	if g.Scope == session.ScopeSession && len(g.Sessions) == 1 {
		return fmt.Sprintf("%s · %s · last %s", g.Sessions[0].ID, g.CWD, humanRelTime(g.Latest))
	}
	return fmt.Sprintf("%d session(s) · %s · last %s",
		len(g.Sessions), humanBytes(g.Bytes), humanRelTime(g.Latest))
}

func displayText(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.Join(strings.Fields(text), " "))
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
	phase    phase
	sessions []session.Session
	scope    session.Scope

	groups   []Group
	visible  []int // indices into groups, current filter
	selected map[int]bool
	cursor   int // position within m.visible
	scroll   int // first visible row of m.visible

	filter textinput.Model
	target textinput.Model
	mode   session.Mode
	width  int
	height int

	// Tab-completion state for the target-path screen.
	targetSuggestions []string
	targetCompleteFor string // input value the suggestions correspond to

	plans   []session.Plan
	reports []session.Report
	runID   string
	err     error
}

// NewModel constructs a ready-to-run TUI model.
func NewModel(mode session.Mode) Model {
	fi := textinput.New()
	fi.Placeholder = "filter by title, session ID, path or agent…"
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
	sessions []session.Session
	err      error
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
		return scanDoneMsg{sessions: all}
	}
}

func groupSessions(sessions []session.Session) []Group {
	bucket := map[string]*Group{}
	for _, s := range sessions {
		key := s.Agent + "\x00" + s.CWD
		g, ok := bucket[key]
		if !ok {
			g = &Group{Agent: s.Agent, CWD: s.CWD, Scope: session.ScopeProject}
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

func (m *Model) rebuildRows() {
	m.groups = nil
	if m.scope == session.ScopeProject {
		m.groups = groupSessions(m.sessions)
	} else {
		for _, s := range m.sessions {
			if s.ProjectOnly {
				continue
			}
			m.groups = append(m.groups, Group{
				Agent: s.Agent, CWD: s.CWD, Sessions: []session.Session{s},
				Latest: s.Modified, Bytes: s.Size,
			})
		}
		sort.SliceStable(m.groups, func(i, j int) bool {
			a, b := m.groups[i], m.groups[j]
			if !a.Latest.Equal(b.Latest) {
				return a.Latest.After(b.Latest)
			}
			return a.Agent+"\x00"+a.CWD+"\x00"+a.Sessions[0].ID < b.Agent+"\x00"+b.CWD+"\x00"+b.Sessions[0].ID
		})
	}
	// Changing scope must never silently expand a session selection to a project.
	m.selected = map[int]bool{}
	m.cursor, m.scroll = 0, 0
	m.err = nil
	m.refilter()
}

// Update is the Bubble Tea reducer.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.adjustScroll()
	case scanDoneMsg:
		m.sessions = msg.sessions
		m.rebuildRows()
		m.err = msg.err
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
		m.adjustScroll()
		return m, nil
	case "down", "ctrl+n":
		if m.cursor < len(m.visible)-1 {
			m.cursor++
		}
		m.adjustScroll()
		return m, nil
	case "pgup":
		step := m.listCapacity()
		m.cursor -= step
		if m.cursor < 0 {
			m.cursor = 0
		}
		m.adjustScroll()
		return m, nil
	case "pgdown":
		step := m.listCapacity()
		m.cursor += step
		if m.cursor >= len(m.visible) {
			m.cursor = max0(len(m.visible) - 1)
		}
		m.adjustScroll()
		return m, nil
	case "g":
		// only act as list-nav when filter is empty; otherwise let it through
		// so the user can type "g" inside the filter.
		if m.filter.Value() == "" {
			m.cursor = 0
			m.adjustScroll()
			return m, nil
		}
	case "G":
		if m.filter.Value() == "" {
			m.cursor = max0(len(m.visible) - 1)
			m.adjustScroll()
			return m, nil
		}
	case "tab":
		if m.scope == session.ScopeSession {
			m.scope = session.ScopeProject
		} else {
			m.scope = session.ScopeSession
		}
		m.rebuildRows()
		return m, nil
	case " ":
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
	case "tab":
		completed, suggestions := completePath(m.target.Value())
		if completed != m.target.Value() {
			m.target.SetValue(completed)
			m.target.SetCursor(len(completed))
			// After accepting a longer prefix, drop the menu so the user
			// can see they made progress; a second Tab repopulates it.
			m.targetSuggestions = nil
			m.targetCompleteFor = ""
		} else if len(suggestions) > 1 {
			m.targetSuggestions = suggestions
			m.targetCompleteFor = m.target.Value()
		} else {
			m.targetSuggestions = nil
			m.targetCompleteFor = ""
		}
		return m, nil
	case "enter":
		newCWD := paths.NormalizeCWD(m.target.Value())
		if newCWD == "" {
			return m, nil
		}
		plans, err := buildPlans(m, newCWD)
		if err != nil {
			m.err = err
			return m, nil
		}
		m.plans = plans
		m.err = nil
		m.phase = phaseConfirm
		return m, nil
	}
	prev := m.target.Value()
	newTarget, cmd := m.target.Update(msg)
	m.target = newTarget
	// Drop stale completion menu the moment the buffer changes.
	if m.target.Value() != prev {
		m.targetSuggestions = nil
		m.targetCompleteFor = ""
	}
	return m, cmd
}

func (m Model) handleConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "n", "esc":
		m.phase = phaseTarget
		return m, nil
	case "y", "enter":
		m.phase = phaseApplying
		plans := m.plans
		return m, func() tea.Msg {
			return runApply(plans)
		}
	case "m":
		m.mode = session.ModeMove
		plans, err := buildPlans(m, m.target.Value())
		if err != nil {
			m.err = err
			m.phase = phaseTarget
			return m, nil
		}
		m.err = nil
		m.plans = plans
		return m, nil
	case "c":
		m.mode = session.ModeCopy
		plans, err := buildPlans(m, m.target.Value())
		if err != nil {
			m.err = err
			m.phase = phaseTarget
			return m, nil
		}
		m.err = nil
		m.plans = plans
		return m, nil
	}
	return m, nil
}

func runApply(plans []session.Plan) applyDoneMsg {
	runID, reports, err := migration.Execute(plans)
	return applyDoneMsg{reports: reports, runID: runID, err: err}
}

func buildPlans(m Model, newCWD string) ([]session.Plan, error) {
	var plans []session.Plan
	var selected []session.Session
	for idx := range m.selected {
		if !m.selected[idx] {
			continue
		}
		selected = append(selected, m.groups[idx].Sessions...)
	}
	for _, g := range groupSessions(selected) {
		a, err := adapter.ByName(g.Agent)
		if err != nil {
			return nil, err
		}
		p, err := a.Plan(g.Sessions, session.PlanOpts{NewCWD: paths.NormalizeCWD(newCWD), Mode: m.mode, Scope: m.scope})
		if err != nil {
			return nil, err
		}
		plans = append(plans, p)
	}
	if err := migration.Validate(plans); err != nil {
		return nil, err
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
	m.scroll = 0
	m.adjustScroll()
}

// listCapacity returns the number of group items that fit in the list view at
// the current terminal height. Each group renders as two lines (title +
// description); the chrome above and below the list is fixed.
func (m Model) listCapacity() int {
	// Frame padding, title, filter, spacing, overflow hint, two help lines,
	// and the operation label occupy ten lines outside the two-line rows.
	const chrome = 10
	if m.height <= 0 {
		return 8 // sensible default before first WindowSizeMsg arrives
	}
	cap := (m.height - chrome) / 2
	if cap < 1 {
		cap = 1
	}
	return cap
}

// adjustScroll keeps the cursor inside the visible window.
func (m *Model) adjustScroll() {
	cap := m.listCapacity()
	if m.cursor < m.scroll {
		m.scroll = m.cursor
	}
	if m.cursor >= m.scroll+cap {
		m.scroll = m.cursor - cap + 1
	}
	if m.scroll < 0 {
		m.scroll = 0
	}
	maxScroll := len(m.visible) - cap
	if maxScroll < 0 {
		maxScroll = 0
	}
	if m.scroll > maxScroll {
		m.scroll = maxScroll
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
	fmt.Fprintln(&b, titleStyle.Render("mvs · "+m.scope.String()+"s")+"  "+dimStyle.Render("tab: switch sessions/projects (clears selection)"))
	fmt.Fprintln(&b, dimStyle.Render("filter:")+" "+m.filter.View())
	fmt.Fprintln(&b, "")
	if len(m.groups) == 0 {
		if m.err != nil {
			fmt.Fprintln(&b, errStyle.Render("scan failed: "+m.err.Error()))
		}
		fmt.Fprintln(&b, dimStyle.Render("no sessions found on this host."))
		fmt.Fprintln(&b, dimStyle.Render("press q to quit."))
		return b.String()
	}
	cap := m.listCapacity()
	start := m.scroll
	end := start + cap
	if end > len(m.visible) {
		end = len(m.visible)
	}
	if start > end {
		start = end
	}
	for vi := start; vi < end; vi++ {
		idx := m.visible[vi]
		g := m.groups[idx]
		marker := "[ ]"
		if m.selected[idx] {
			marker = selectedStyle.Render("[x]")
		}
		pointer := "  "
		title, description := displayText(g.Title()), displayText(g.Description())
		if m.width > 12 {
			rowStyle := lipgloss.NewStyle().MaxWidth(m.width - 12)
			title, description = rowStyle.Render(title), rowStyle.Render(description)
		}
		line := fmt.Sprintf(" %s %s\n      %s",
			marker,
			title,
			dimStyle.Render(description))
		if vi == m.cursor {
			pointer = cursorStyle.Render("▸ ")
			line = cursorStyle.Render(line)
		}
		fmt.Fprint(&b, pointer)
		fmt.Fprintln(&b, line)
	}
	fmt.Fprintln(&b, "")
	if len(m.visible) > end {
		fmt.Fprintln(&b, dimStyle.Render(fmt.Sprintf(
			"… %d more below (pgdn / G to jump)", len(m.visible)-end)))
	} else if start > 0 {
		fmt.Fprintln(&b, dimStyle.Render(fmt.Sprintf(
			"… %d more above (pgup / g to jump)", start)))
	} else {
		fmt.Fprintln(&b, "")
	}
	fmt.Fprintln(&b, dimStyle.Render(
		"↑/↓ pgup/pgdn g/G  space: select  enter: continue"))
	fmt.Fprintln(&b, dimStyle.Render("ctrl+a: all filtered  ctrl+x: clear  q: quit"))
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
	fmt.Fprintln(&b, dimStyle.Render(fmt.Sprintf("%d %s(s) selected.", count, m.scope.String())))
	fmt.Fprintln(&b, "")
	fmt.Fprintln(&b, "Enter the new cwd (absolute path, ~ supported):")
	fmt.Fprintln(&b, m.target.View())
	if len(m.targetSuggestions) > 0 {
		fmt.Fprintln(&b, "")
		fmt.Fprintln(&b, dimStyle.Render(fmt.Sprintf("%d candidates:", len(m.targetSuggestions))))
		for _, s := range m.targetSuggestions {
			fmt.Fprintln(&b, "  "+accentStyle.Render(s))
		}
	}
	fmt.Fprintln(&b, "")
	fmt.Fprintln(&b, dimStyle.Render("tab: complete  enter: confirm  esc: back  ctrl+c: quit"))
	if m.err != nil {
		fmt.Fprintln(&b, errStyle.Render("error: "+m.err.Error()))
	}
	return b.String()
}

func (m Model) viewConfirm() string {
	var b strings.Builder
	fmt.Fprintln(&b, titleStyle.Render("mvs · confirm"))
	fmt.Fprintln(&b, "")
	fmt.Fprintf(&b, "selection: %s · operation: %s\n", m.scope.String(), accentStyle.Render(strings.ToUpper(m.mode.String())))
	fmt.Fprintln(&b, "")
	for _, p := range m.plans {
		fmt.Fprintf(&b, "%s  %s → %s\n",
			accentStyle.Render("["+p.Agent+"]"),
			p.OldCWD, p.NewCWD)
		if p.Scope == session.ScopeSession.String() {
			fmt.Fprintf(&b, "    selected sessions: %s\n", strings.Join(p.SessionIDs, ", "))
		}
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

// completePath returns the bash-style tab completion of input. It returns the
// completed input (longest common prefix when there are multiple matches) and
// — when more than one match remains — the candidate basenames sorted
// alphabetically for display below the prompt.
//
// Empty input returns unchanged with no candidates. Tilde at the start is
// expanded for the filesystem lookup but collapsed back into the result.
func completePath(input string) (string, []string) {
	if input == "" {
		return input, nil
	}
	sep := string(os.PathSeparator)
	idx := strings.LastIndex(input, sep)
	var prefix, frag string
	if idx < 0 {
		prefix, frag = "", input
	} else {
		prefix, frag = input[:idx+1], input[idx+1:]
	}
	// Resolve the directory we'll list.
	dir := expandTilde(prefix)
	if dir == "" {
		dir = "."
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return input, nil
	}
	var matches []string
	isDir := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		// Match bash: hide dotfiles unless the user is explicitly typing one.
		if frag == "" && strings.HasPrefix(name, ".") {
			continue
		}
		if strings.HasPrefix(name, frag) {
			matches = append(matches, name)
			isDir[name] = e.IsDir()
		}
	}
	if len(matches) == 0 {
		return input, nil
	}
	sort.Strings(matches)
	if len(matches) == 1 {
		result := prefix + matches[0]
		if isDir[matches[0]] {
			result += sep
		}
		return result, nil
	}
	lcp := longestCommonPrefix(matches)
	completed := prefix + lcp
	if completed == input {
		// Buffer is already at the longest common prefix; surface candidates.
		return input, matches
	}
	return completed, matches
}

// expandTilde turns "~" or "~/foo" into "$HOME" / "$HOME/foo". Anything else
// is returned unchanged.
func expandTilde(p string) string {
	if p == "~" || strings.HasPrefix(p, "~"+string(os.PathSeparator)) {
		home, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		if p == "~" {
			return home
		}
		return filepath.Join(home, p[2:])
	}
	return p
}

func longestCommonPrefix(s []string) string {
	if len(s) == 0 {
		return ""
	}
	p := s[0]
	for _, x := range s[1:] {
		for !strings.HasPrefix(x, p) {
			p = p[:len(p)-1]
			if p == "" {
				return ""
			}
		}
	}
	return p
}
