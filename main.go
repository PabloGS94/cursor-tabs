package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

var (
	dimStyle    = lipgloss.NewStyle().Faint(true)
	greenStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	yellowStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	redStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	cyanStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)
	tabStyle    = lipgloss.NewStyle().Faint(true)
	tabSelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true).Underline(true)
)

// statusColWidth fits the longest label, "Needs input".
const statusColWidth = 11

// tmuxSocket isolates cursor-tabs sessions on their own tmux server, so key
// bindings (like Ctrl+Q to detach) never affect the user's regular tmux.
const tmuxSocket = "cursortabs"

func tmuxCmd(args ...string) *exec.Cmd {
	return exec.Command("tmux", append([]string{"-L", tmuxSocket}, args...)...)
}

type session struct {
	Name     string // tmux session name
	Slot     int    // numeric suffix used for naming; 0 for legacy unsuffixed
	Status   string
	Summary  string
	Activity time.Time
}

type project struct {
	Name     string
	Path     string
	Prefix   string // tmux session name prefix, e.g. cta_giftcards
	Sessions []session
}

type model struct {
	projects      []project
	tab           int // selected project
	row           int // selected session within the project
	agentCmd      string
	statusMsg     string
	ready         bool
	width         int
	height        int
	pendingKill   string // session name armed for deletion by first ctrl+x
	pendingKillAt time.Time
}

// pendingKillTimeout is how long a ctrl+x confirmation stays armed.
const pendingKillTimeout = 3 * time.Second

type tickMsg time.Time

type actionDoneMsg struct {
	status string
	err    error
}

type attachDoneMsg struct {
	err error
}

func main() {
	agentCmd, err := detectAgentCommand()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cursor-tabs: %v\n", err)
		os.Exit(1)
	}
	if err := requireCommand("tmux"); err != nil {
		fmt.Fprintf(os.Stderr, "cursor-tabs: %v\n", err)
		os.Exit(1)
	}

	projects, err := loadProjects()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cursor-tabs: %v\n", err)
		os.Exit(1)
	}
	if len(projects) == 0 {
		fmt.Fprintln(os.Stderr, "cursor-tabs: no repos found. set CURSOR_TABS_REPOS or CURSOR_TABS_ROOT")
		os.Exit(1)
	}
	assignSessionPrefixes(projects)

	m := model{
		projects: projects,
		agentCmd: agentCmd,
	}
	m.refreshState()

	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "cursor-tabs: %v\n", err)
		os.Exit(1)
	}
}

func detectAgentCommand() (string, error) {
	for _, candidate := range []string{"agent", "cursor-agent"} {
		if _, err := exec.LookPath(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", errors.New("could not find cursor agent CLI (expected `agent` or `cursor-agent`)")
}

func requireCommand(name string) error {
	if _, err := exec.LookPath(name); err != nil {
		return fmt.Errorf("required command `%s` not found on PATH", name)
	}
	return nil
}

func loadProjects() ([]project, error) {
	reposEnv := strings.TrimSpace(os.Getenv("CURSOR_TABS_REPOS"))
	if reposEnv != "" {
		return projectsFromEnv(reposEnv)
	}

	root := strings.TrimSpace(os.Getenv("CURSOR_TABS_ROOT"))
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		root = filepath.Join(home, "dev")
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}

	var projects []project
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		repoPath := filepath.Join(root, entry.Name())
		if isGitRepo(repoPath) {
			projects = append(projects, project{Name: entry.Name(), Path: repoPath})
		}
	}

	sortProjects(projects)
	return projects, nil
}

func projectsFromEnv(raw string) ([]project, error) {
	parts := strings.Split(raw, ",")
	var projects []project
	for _, p := range parts {
		path := strings.TrimSpace(p)
		if path == "" {
			continue
		}
		abs, err := filepath.Abs(expandHome(path))
		if err != nil {
			return nil, err
		}
		if !isGitRepo(abs) {
			return nil, fmt.Errorf("not a git repo: %s", abs)
		}
		projects = append(projects, project{Name: filepath.Base(abs), Path: abs})
	}
	sortProjects(projects)
	return projects, nil
}

func sortProjects(projects []project) {
	sort.Slice(projects, func(i, j int) bool {
		di, dj := filepath.Dir(projects[i].Path), filepath.Dir(projects[j].Path)
		if di != dj {
			return di < dj
		}
		return projects[i].Name < projects[j].Name
	})
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

func isGitRepo(path string) bool {
	_, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil
}

func assignSessionPrefixes(projects []project) {
	used := map[string]int{}
	for i := range projects {
		base := sanitize("cta_" + projects[i].Name)
		count := used[base]
		used[base] = count + 1
		if count > 0 {
			base = fmt.Sprintf("%s~%d", base, count+1)
		}
		projects[i].Prefix = base
	}
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
			continue
		}
		b.WriteRune('_')
	}
	return b.String()
}

// parseSlot reports whether tmux session `name` belongs to a project with
// `prefix`, and which numeric slot it occupies. A bare prefix (from older
// versions) counts as slot 0.
func parseSlot(name, prefix string) (int, bool) {
	if name == prefix {
		return 0, true
	}
	rest, ok := strings.CutPrefix(name, prefix+"_")
	if !ok {
		return 0, false
	}
	slot, err := strconv.Atoi(rest)
	if err != nil || slot < 1 {
		return 0, false
	}
	return slot, true
}

func (m model) Init() tea.Cmd {
	return tickCmd()
}

func tickCmd() tea.Cmd {
	return tea.Tick(1500*time.Millisecond, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m model) currentProject() *project {
	return &m.projects[m.tab]
}

func (m model) currentSession() *session {
	p := m.currentProject()
	if len(p.Sessions) == 0 {
		return nil
	}
	if m.row >= len(p.Sessions) {
		return &p.Sessions[len(p.Sessions)-1]
	}
	return &p.Sessions[m.row]
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ready = true
		return m, nil
	case tickMsg:
		if m.pendingKill != "" && time.Since(m.pendingKillAt) > pendingKillTimeout {
			m.pendingKill = ""
		}
		m.refreshState()
		return m, tickCmd()
	case actionDoneMsg:
		if msg.err != nil {
			m.statusMsg = fmt.Sprintf("error: %v", msg.err)
		} else if msg.status != "" {
			m.statusMsg = msg.status
		}
		m.refreshState()
		return m, nil
	case attachDoneMsg:
		if msg.err != nil {
			m.statusMsg = fmt.Sprintf("attach error: %v", msg.err)
		}
		m.refreshState()
		return m, nil
	case tea.KeyMsg:
		// Any key other than a repeat ctrl+x disarms a pending delete.
		if msg.String() != "ctrl+x" {
			m.pendingKill = ""
		}
		switch msg.String() {
		case "ctrl+c", "q":
			return m, tea.Quit
		case "left", "h":
			m.tab = (m.tab - 1 + len(m.projects)) % len(m.projects)
			m.row = 0
			return m, nil
		case "right", "l":
			m.tab = (m.tab + 1) % len(m.projects)
			m.row = 0
			return m, nil
		case "up", "k":
			if n := len(m.currentProject().Sessions); n > 0 {
				m.row = (m.row - 1 + n) % n
			}
			return m, nil
		case "down", "j":
			if n := len(m.currentProject().Sessions); n > 0 {
				m.row = (m.row + 1) % n
			}
			return m, nil
		case "r":
			m.refreshState()
			m.statusMsg = "refreshed"
			return m, nil
		case "n":
			return m.startAndAttach()
		case "ctrl+x":
			s := m.currentSession()
			if s == nil {
				m.statusMsg = "no session to stop"
				return m, nil
			}
			if m.pendingKill == s.Name && time.Since(m.pendingKillAt) <= pendingKillTimeout {
				m.pendingKill = ""
				name := s.Name
				label := fmt.Sprintf("%s #%d", m.currentProject().Name, m.row+1)
				return m, runAction(func() (string, error) {
					return stopSession(name, label)
				})
			}
			m.pendingKill = s.Name
			m.pendingKillAt = time.Now()
			return m, nil
		case "enter":
			if s := m.currentSession(); s != nil {
				return m, m.attachCmd(s.Name)
			}
			return m.startAndAttach()
		}
	}
	return m, nil
}

// startAndAttach creates a new session in the current project's lowest free
// slot and attaches to it immediately.
func (m model) startAndAttach() (tea.Model, tea.Cmd) {
	p := m.currentProject()
	name, err := startSession(p, m.agentCmd)
	if err != nil {
		m.statusMsg = fmt.Sprintf("error: %v", err)
		return m, nil
	}
	m.refreshState()
	for i, s := range p.Sessions {
		if s.Name == name {
			m.row = i
			break
		}
	}
	return m, m.attachCmd(name)
}

func runAction(fn func() (string, error)) tea.Cmd {
	return func() tea.Msg {
		status, err := fn()
		return actionDoneMsg{status: status, err: err}
	}
}

func (m model) attachCmd(sessionName string) tea.Cmd {
	cmd := tmuxCmd("attach-session", "-t", sessionName)
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		return attachDoneMsg{err: err}
	})
}

func startSession(p *project, agentCmd string) (string, error) {
	name := nextSessionName(p)
	cmd := tmuxCmd("new-session", "-d", "-s", name, "-c", p.Path, agentCmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("start failed: %s", strings.TrimSpace(string(out)))
	}
	// One-key detach: plain Ctrl+Q returns to cursor-tabs. Safe to bind
	// globally because this tmux server only hosts cursor-tabs sessions.
	tmuxCmd("bind-key", "-n", "C-q", "detach-client").Run()
	// Show a persistent hint inside the session on how to get back.
	tmuxCmd("set-option", "-t", name, "status-right",
		" ctrl+q = back to cursor-tabs ").Run()
	return name, nil
}

func nextSessionName(p *project) string {
	taken := map[int]bool{}
	for _, s := range p.Sessions {
		taken[s.Slot] = true
	}
	slot := 1
	for taken[slot] {
		slot++
	}
	return fmt.Sprintf("%s_%d", p.Prefix, slot)
}

func stopSession(name, label string) (string, error) {
	cmd := tmuxCmd("kill-session", "-t", name)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("stop failed: %s", strings.TrimSpace(string(out)))
	}
	return fmt.Sprintf("stopped %s", label), nil
}

// liveSessions returns all sessions on the cursor-tabs tmux server with their
// last-activity timestamps. An empty map means the server isn't running.
func liveSessions() map[string]time.Time {
	live := map[string]time.Time{}
	out, err := tmuxCmd("list-sessions", "-F", "#{session_name}\t#{session_activity}").Output()
	if err != nil {
		return live
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) != 2 {
			continue
		}
		sec, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			continue
		}
		live[parts[0]] = time.Unix(sec, 0)
	}
	return live
}

func (m *model) refreshState() {
	live := liveSessions()
	for i := range m.projects {
		p := &m.projects[i]
		var sessions []session
		for name, activity := range live {
			slot, ok := parseSlot(name, p.Prefix)
			if !ok {
				continue
			}
			s := session{Name: name, Slot: slot, Activity: activity}
			out, err := tmuxCmd("capture-pane", "-p", "-t", name, "-S", "-30").CombinedOutput()
			if err != nil {
				s.Status = "error"
				s.Summary = strings.TrimSpace(string(out))
			} else {
				text := string(out)
				s.Status = detectStatus(text)
				s.Summary = lastMeaningfulLine(text)
			}
			sessions = append(sessions, s)
		}
		sort.Slice(sessions, func(a, b int) bool {
			return sessions[a].Slot < sessions[b].Slot
		})
		p.Sessions = sessions
	}
	if n := len(m.currentProject().Sessions); n == 0 {
		m.row = 0
	} else if m.row >= n {
		m.row = n - 1
	}
}

func detectStatus(output string) string {
	l := strings.ToLower(output)
	switch {
	case strings.Contains(l, "error"), strings.Contains(l, "failed"):
		return "error"
	case strings.Contains(l, "awaiting input"),
		strings.Contains(l, "needs input"),
		strings.Contains(l, "permission"),
		strings.Contains(l, "do you want"):
		return "waiting"
	case strings.Contains(l, "working"),
		strings.Contains(l, "running"),
		strings.Contains(l, "thinking"),
		strings.Contains(l, "implementing"):
		return "running"
	default:
		return "idle"
	}
}

// lastMeaningfulLine picks the most recent line of real content, skipping
// prompt chrome (input box, model footer, path footer) from the agent UI.
func lastMeaningfulLine(text string) string {
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := collapseSpaces(strings.TrimSpace(lines[i]))
		if line == "" || !hasLetterOrDigit(line) {
			continue
		}
		l := strings.ToLower(line)
		if strings.Contains(l, "add a follow-up") ||
			strings.Contains(l, "shortcuts") ||
			strings.HasPrefix(line, "~") ||
			(strings.Contains(line, "·") && strings.Contains(line, "%")) {
			continue
		}
		return line
	}
	return ""
}

func hasLetterOrDigit(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

func collapseSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func formatAge(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func statusLabel(status string) string {
	padded := func(s string) string {
		if len(s) < statusColWidth {
			return s + strings.Repeat(" ", statusColWidth-len(s))
		}
		return s
	}
	switch status {
	case "running":
		return greenStyle.Render(padded("Working"))
	case "waiting":
		return yellowStyle.Render(padded("Needs input"))
	case "idle":
		return greenStyle.Render(padded("Idle"))
	case "error":
		return redStyle.Render(padded("Error"))
	default:
		return dimStyle.Render(padded("Stopped"))
	}
}

// tabDot summarizes a project's sessions as a colored dot for the tab bar.
func tabDot(p project) string {
	if len(p.Sessions) == 0 {
		return ""
	}
	hasWaiting, hasRunning, hasError := false, false, false
	for _, s := range p.Sessions {
		switch s.Status {
		case "waiting":
			hasWaiting = true
		case "running":
			hasRunning = true
		case "error":
			hasError = true
		}
	}
	switch {
	case hasError:
		return " " + redStyle.Render("●")
	case hasWaiting:
		return " " + yellowStyle.Render("●")
	case hasRunning:
		return " " + greenStyle.Render("●")
	default:
		return " " + dimStyle.Render("●")
	}
}

func (m model) renderTabs() string {
	rendered := make([]string, len(m.projects))
	for i, p := range m.projects {
		label := p.Name
		if i == m.tab {
			label = tabSelStyle.Render(label)
		} else {
			label = tabStyle.Render(label)
		}
		rendered[i] = label + tabDot(p)
	}

	sep := dimStyle.Render("  ·  ")
	sepWidth := lipgloss.Width(sep)

	// Scroll the tab window so the selected tab is always fully visible.
	start := 0
	for {
		width := 0
		fits := false
		for i := start; i < len(rendered); i++ {
			if i > start {
				width += sepWidth
			}
			width += lipgloss.Width(rendered[i])
			if i == m.tab {
				fits = width <= m.width-2
				break
			}
			if width > m.width-2 {
				break
			}
		}
		if fits || start >= m.tab {
			break
		}
		start++
	}

	var b strings.Builder
	if start > 0 {
		b.WriteString(dimStyle.Render("‹ "))
	}
	width := lipgloss.Width(b.String())
	for i := start; i < len(rendered); i++ {
		chunk := rendered[i]
		chunkWidth := lipgloss.Width(chunk)
		if i > start {
			chunkWidth += sepWidth
		}
		if width+chunkWidth > m.width-2 {
			b.WriteString(dimStyle.Render(" ›"))
			break
		}
		if i > start {
			b.WriteString(sep)
		}
		b.WriteString(chunk)
		width += chunkWidth
	}
	return b.String()
}

func (m model) View() string {
	if !m.ready {
		return "loading..."
	}

	var b strings.Builder

	b.WriteString(m.renderTabs())
	b.WriteString("\n\n")

	p := m.currentProject()
	if len(p.Sessions) == 0 {
		b.WriteString(dimStyle.Render("  no sessions — enter or n to start one"))
		b.WriteString("\n")
	} else {
		for i := range p.Sessions {
			b.WriteString(m.renderSessionRow(i))
			b.WriteString("\n")
		}
	}

	b.WriteString("\n")
	b.WriteString(dimStyle.Render("←→ project · ↑↓ session · enter open · n new · ctrl+x stop · ctrl+q back here · q quit"))
	if m.statusMsg != "" {
		b.WriteString("\n")
		b.WriteString(dimStyle.Render(m.statusMsg))
	}
	return b.String()
}

func (m model) renderSessionRow(i int) string {
	p := m.currentProject()
	s := p.Sessions[i]
	selected := i == m.row

	marker := "  "
	if selected {
		marker = cyanStyle.Render("❯ ")
	}

	num := fmt.Sprintf("%d", i+1)
	if selected {
		num = cyanStyle.Render(num)
	} else {
		num = dimStyle.Render(num)
	}

	row := marker + num + "  " + statusLabel(s.Status)

	age := formatAge(s.Activity)

	if s.Name == m.pendingKill {
		return row + " " + redStyle.Render("ctrl+x again to delete")
	}

	summary := s.Summary
	if summary != "" && m.width > 0 {
		avail := m.width - lipgloss.Width(row) - len(age) - 8
		if avail > 10 {
			if len([]rune(summary)) > avail {
				summary = string([]rune(summary)[:avail-1]) + "…"
			}
			row += dimStyle.Render(" · " + summary)
		}
	}

	if age != "" && m.width > 0 {
		gap := m.width - lipgloss.Width(row) - len(age) - 1
		if gap < 1 {
			gap = 1
		}
		row += strings.Repeat(" ", gap) + dimStyle.Render(age)
	}

	return row
}
