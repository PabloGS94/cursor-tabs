package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
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
	Title    string // session purpose, stored as tmux option @cta_title
	Status   string
	Summary  string
	Activity time.Time
	Created  time.Time
}

type project struct {
	Name     string
	Path     string
	Prefix   string // tmux session name prefix, e.g. cta_giftcards
	Sessions []session
}

type viewMode int

const (
	viewDashboard viewMode = iota
	viewPicker
)

// pickerState backs the interactive "which projects show as tabs" screen.
type pickerState struct {
	candidates []project
	checked    []bool
	sessions   []int // live session count per candidate at open time
	cursor     int
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
	view          viewMode
	picker        pickerState
}

// pendingKillTimeout is how long a ctrl+x confirmation stays armed.
const pendingKillTimeout = 3 * time.Second

// maxTitleLen caps session titles, both on input and display.
const maxTitleLen = 40

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

	all, err := discoverRepos()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cursor-tabs: %v\n", err)
		os.Exit(1)
	}
	if len(all) == 0 {
		fmt.Fprintln(os.Stderr, "cursor-tabs: no folders found. set CURSOR_TABS_REPOS or CURSOR_TABS_ROOT")
		os.Exit(1)
	}
	projects := visibleProjects(all, loadConfig().Repos)
	assignSessionPrefixes(projects)

	m := model{
		projects: projects,
		agentCmd: agentCmd,
	}
	m.refreshState()
	// Bring status bars of sessions started by older versions up to date.
	for _, p := range m.projects {
		for _, s := range p.Sessions {
			updateStatusBar(p.Name, s)
		}
	}

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

type config struct {
	Repos []string `json:"repos"`
}

func configPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "cursor-tabs", "config.json"), nil
}

func loadConfig() config {
	var cfg config
	path, err := configPath()
	if err != nil {
		return cfg
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	json.Unmarshal(data, &cfg)
	return cfg
}

func saveConfig(cfg config) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// visibleProjects filters discovered repos down to the config selection.
// An empty selection (no config yet) or one that matches nothing shows all.
func visibleProjects(all []project, selected []string) []project {
	if len(selected) == 0 {
		return all
	}
	want := map[string]bool{}
	for _, p := range selected {
		want[p] = true
	}
	var out []project
	for _, p := range all {
		if want[p.Path] {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return all
	}
	return out
}

// discoverRepos finds all candidate git repos, from CURSOR_TABS_REPOS if set,
// otherwise by scanning the root dir (CURSOR_TABS_ROOT or ~/dev).
func discoverRepos() ([]project, error) {
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
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		projects = append(projects, project{Name: entry.Name(), Path: filepath.Join(root, entry.Name())})
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
		if info, err := os.Stat(abs); err != nil || !info.IsDir() {
			return nil, fmt.Errorf("not a directory: %s", abs)
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
		if m.view == viewPicker {
			return m.handlePickerKey(msg)
		}
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
		case "p":
			m.openPicker()
			return m, nil
		case "ctrl+x":
			s := m.currentSession()
			if s == nil {
				m.statusMsg = "no session to stop"
				return m, nil
			}
			if m.pendingKill == s.Name && time.Since(m.pendingKillAt) <= pendingKillTimeout {
				m.pendingKill = ""
				name := s.Name
				label := fmt.Sprintf("%s #%d", m.currentProject().Name, s.Slot)
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

func (m *model) openPicker() {
	candidates, err := discoverRepos()
	if err != nil || len(candidates) == 0 {
		m.statusMsg = "could not discover repos"
		return
	}
	assignSessionPrefixes(candidates)

	visible := map[string]bool{}
	for _, p := range m.projects {
		visible[p.Path] = true
	}

	live := liveSessions()
	checked := make([]bool, len(candidates))
	sessions := make([]int, len(candidates))
	cursor := 0
	for i, c := range candidates {
		checked[i] = visible[c.Path]
		for name := range live {
			if _, ok := parseSlot(name, c.Prefix); ok {
				sessions[i]++
			}
		}
		if c.Path == m.currentProject().Path {
			cursor = i
		}
	}

	m.picker = pickerState{
		candidates: candidates,
		checked:    checked,
		sessions:   sessions,
		cursor:     cursor,
	}
	m.view = viewPicker
	m.statusMsg = ""
}

func (m model) handlePickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	pk := &m.picker
	n := len(pk.candidates)
	switch msg.String() {
	case "up", "k":
		pk.cursor = (pk.cursor - 1 + n) % n
	case "down", "j":
		pk.cursor = (pk.cursor + 1) % n
	case " ":
		pk.checked[pk.cursor] = !pk.checked[pk.cursor]
	case "enter":
		return m.savePicker()
	case "esc", "q", "ctrl+c":
		m.view = viewDashboard
	}
	return m, nil
}

func (m model) savePicker() (tea.Model, tea.Cmd) {
	var selected []project
	var paths []string
	for i, c := range m.picker.candidates {
		if m.picker.checked[i] {
			selected = append(selected, c)
			paths = append(paths, c.Path)
		}
	}
	if len(selected) == 0 {
		m.statusMsg = "select at least one project"
		return m, nil
	}
	if err := saveConfig(config{Repos: paths}); err != nil {
		m.statusMsg = fmt.Sprintf("could not save config: %v", err)
		return m, nil
	}

	prevPath := m.currentProject().Path
	m.projects = selected
	assignSessionPrefixes(m.projects)
	m.tab = 0
	for i, p := range m.projects {
		if p.Path == prevPath {
			m.tab = i
			break
		}
	}
	m.row = 0
	m.view = viewDashboard
	m.statusMsg = fmt.Sprintf("showing %d projects", len(selected))
	m.refreshState()
	return m, nil
}

// applyAutoTitles fills in session titles. Preferred source is the Cursor
// CLI's own chat title (covers both /rename and Cursor's auto-generated
// names); fallback is scraping the first prompt from the scrollback.
func applyAutoTitles(p *project, sessions []session) {
	if len(sessions) == 0 {
		return
	}
	chats := projectChats(p.Path)

	// Assign each chat to the session that was most recently created before
	// the chat started (the agent opens its chat moments after the tmux
	// session starts). Keep only the most recently updated chat per session.
	best := map[int]chatMeta{}
	for _, c := range chats {
		idx := -1
		for i := range sessions {
			if sessions[i].Created.Unix() <= c.CreatedAtMs/1000+5 &&
				(idx == -1 || sessions[i].Created.After(sessions[idx].Created)) {
				idx = i
			}
		}
		if idx < 0 {
			continue
		}
		if b, ok := best[idx]; !ok || c.UpdatedAtMs > b.UpdatedAtMs {
			best[idx] = c
		}
	}

	for i := range sessions {
		s := &sessions[i]
		if c, ok := best[i]; ok && strings.TrimSpace(c.Title) != "" {
			title := truncateTitle(strings.TrimSpace(c.Title))
			if title != s.Title {
				s.Title = title
				setSessionTitle(s.Name, title)
				updateStatusBar(p.Name, *s)
			}
			continue
		}
		if s.Title == "" {
			if title := autoTitle(s.Name); title != "" {
				s.Title = title
				setSessionTitle(s.Name, title)
				updateStatusBar(p.Name, *s)
			}
		}
	}
}

func truncateTitle(s string) string {
	if r := []rune(s); len(r) > maxTitleLen {
		return string(r[:maxTitleLen-1]) + "…"
	}
	return s
}

// autoTitle derives a session title from the first user prompt. It only
// trusts the scrollback when the "Cursor Agent" welcome header is still
// visible, which guarantees we are looking at the true start of the
// conversation rather than a scrolled-off middle.
func autoTitle(sessionName string) string {
	// -J joins wrapped lines so the header/tip lines stay single lines.
	out, err := tmuxCmd("capture-pane", "-p", "-J", "-t", sessionName, "-S", "-").Output()
	if err != nil {
		return ""
	}
	sawHeader := false
	for _, raw := range strings.Split(string(out), "\n") {
		line := collapseSpaces(strings.TrimSpace(raw))
		if line == "" {
			continue
		}
		// The input-box border marks the end of the transcript; a
		// half-typed draft below it must never become the title.
		if strings.ContainsAny(line, "▄▀") {
			break
		}
		if line == "Cursor Agent" {
			sawHeader = true
			continue
		}
		if !sawHeader || !hasLetterOrDigit(line) {
			continue
		}
		if strings.HasPrefix(line, "v20") || strings.HasPrefix(line, "Tip:") {
			continue
		}
		return truncateTitle(line)
	}
	return ""
}

func setSessionTitle(sessionName, title string) {
	tmuxCmd("set-option", "-t", sessionName, "@cta_title", title).Run()
}

func getSessionTitle(sessionName string) string {
	out, err := tmuxCmd("show-option", "-t", sessionName, "-qv", "@cta_title").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// updateStatusBar sets a readable label in the tmux status bar bottom-left,
// e.g. " giftcards #1 — checkout bugfix ".
func updateStatusBar(projectName string, s session) {
	label := fmt.Sprintf(" %s #%d", projectName, s.Slot)
	if s.Title != "" {
		label += " — " + s.Title
	}
	label += " "
	tmuxCmd("set-option", "-t", s.Name, "status-left-length", "60").Run()
	tmuxCmd("set-option", "-t", s.Name, "status-left", label).Run()
	// Hide the default window list ("0:node*"); the label says it all.
	tmuxCmd("set-option", "-t", s.Name, "window-status-format", "").Run()
	tmuxCmd("set-option", "-t", s.Name, "window-status-current-format", "").Run()
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
	// Pass real mouse/scroll events through to the agent TUI. Without this,
	// terminals convert trackpad scrolling into arrow keys, which the agent
	// interprets as prompt-history navigation.
	tmuxCmd("set-option", "-g", "mouse", "on").Run()
	// Show a persistent hint inside the session on how to get back.
	tmuxCmd("set-option", "-t", name, "status-right",
		" ctrl+q = back to cursor-tabs ").Run()
	slot, _ := parseSlot(name, p.Prefix)
	updateStatusBar(p.Name, session{Name: name, Slot: slot})
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

type liveInfo struct {
	activity time.Time
	created  time.Time
}

// liveSessions returns all sessions on the cursor-tabs tmux server with their
// timestamps. An empty map means the server isn't running.
func liveSessions() map[string]liveInfo {
	live := map[string]liveInfo{}
	out, err := tmuxCmd("list-sessions", "-F", "#{session_name}\t#{session_activity}\t#{session_created}").Output()
	if err != nil {
		return live
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) != 3 {
			continue
		}
		activity, err1 := strconv.ParseInt(parts[1], 10, 64)
		created, err2 := strconv.ParseInt(parts[2], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		live[parts[0]] = liveInfo{activity: time.Unix(activity, 0), created: time.Unix(created, 0)}
	}
	return live
}

// chatMeta mirrors the fields we need from the Cursor CLI's chat metadata
// files at ~/.cursor/chats/<md5 of cwd>/<chat-id>/meta.json.
type chatMeta struct {
	Title       string `json:"title"`
	CreatedAtMs int64  `json:"createdAtMs"`
	UpdatedAtMs int64  `json:"updatedAtMs"`
}

// projectChats lists Cursor CLI chats started from the project directory.
func projectChats(projectPath string) []chatMeta {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	sum := md5.Sum([]byte(projectPath))
	dir := filepath.Join(home, ".cursor", "chats", hex.EncodeToString(sum[:]))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var chats []chatMeta
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name(), "meta.json"))
		if err != nil {
			continue
		}
		var m chatMeta
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		chats = append(chats, m)
	}
	return chats
}

func (m *model) refreshState() {
	live := liveSessions()
	for i := range m.projects {
		p := &m.projects[i]
		var sessions []session
		for name, info := range live {
			slot, ok := parseSlot(name, p.Prefix)
			if !ok {
				continue
			}
			s := session{Name: name, Slot: slot, Activity: info.activity, Created: info.created}
			s.Title = getSessionTitle(name)
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
		applyAutoTitles(p, sessions)
		p.Sessions = sessions
	}
	if n := len(m.currentProject().Sessions); n == 0 {
		m.row = 0
	} else if m.row >= n {
		m.row = n - 1
	}
}

// detectStatus reads the Cursor CLI's UI chrome rather than the conversation
// text, so prose mentioning "error" or "working" can't skew the status.
func detectStatus(output string) string {
	l := strings.ToLower(output)

	// While the agent runs, the CLI shows a braille spinner and an
	// "esc to interrupt" hint. Neither appears in conversation prose.
	if strings.Contains(l, "esc to interrupt") ||
		strings.Contains(l, "esc to cancel") ||
		containsBraille(output) {
		return "running"
	}

	// The input-box placeholder is only visible when the agent is ready.
	if strings.Contains(l, "add a follow-up") {
		return "idle"
	}

	// No placeholder and no spinner: likely an interactive dialog
	// (permissions, confirmations) replacing the input box.
	if strings.Contains(l, "do you want") ||
		strings.Contains(l, "y/n") ||
		strings.Contains(l, "permission") ||
		strings.Contains(l, "select an option") ||
		strings.Contains(l, "enter to confirm") {
		return "waiting"
	}

	// Structured error markers at the start of a line (agent crashed or
	// the CLI printed a hard error), never the word mid-sentence.
	for _, raw := range strings.Split(output, "\n") {
		t := strings.TrimSpace(raw)
		if strings.HasPrefix(t, "Error:") || strings.HasPrefix(t, "✗") || strings.HasPrefix(t, "✘") {
			return "error"
		}
	}

	return "idle"
}

// containsBraille reports whether s contains braille pattern characters,
// which the Cursor CLI uses for its activity spinner.
func containsBraille(s string) bool {
	for _, r := range s {
		if r >= 0x2800 && r <= 0x28FF {
			return true
		}
	}
	return false
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
	if m.view == viewPicker {
		return m.pickerView()
	}
	return m.dashboardView()
}

// withBottomFooter pads between content and footer so the footer sits on the
// last lines of the terminal.
func (m model) withBottomFooter(content, footer string) string {
	pad := m.height - lipgloss.Height(content) - lipgloss.Height(footer) + 1
	if pad < 1 {
		pad = 1
	}
	return content + strings.Repeat("\n", pad) + footer
}

func (m model) dashboardView() string {
	var b strings.Builder

	b.WriteString(m.renderTabs())
	b.WriteString("\n\n")

	p := m.currentProject()
	if len(p.Sessions) == 0 {
		b.WriteString(dimStyle.Render("  no sessions — enter or n to start one"))
	} else {
		titleWidth := m.titleColWidth()
		for i := range p.Sessions {
			if i > 0 {
				b.WriteString("\n")
			}
			b.WriteString(m.renderSessionRow(i, titleWidth))
		}
	}

	var f strings.Builder
	if m.statusMsg != "" {
		f.WriteString(dimStyle.Render(m.statusMsg))
		f.WriteString("\n")
	}
	f.WriteString(dimStyle.Render("←→ project · ↑↓ session · enter open · n new · p projects · ctrl+x stop · ctrl+q back here · q quit"))

	return m.withBottomFooter(b.String(), f.String())
}

func (m model) pickerView() string {
	var b strings.Builder

	b.WriteString(boldTitle("Select projects to show as tabs"))
	b.WriteString("\n\n")

	hiddenRunning := 0
	for i, c := range m.picker.candidates {
		if !m.picker.checked[i] && m.picker.sessions[i] > 0 {
			hiddenRunning += m.picker.sessions[i]
		}
		if i > 0 {
			b.WriteString("\n")
		}

		marker := "  "
		if i == m.picker.cursor {
			marker = cyanStyle.Render("❯ ")
		}
		box := "[ ]"
		if m.picker.checked[i] {
			box = "[x]"
		}
		name := c.Name
		if i == m.picker.cursor {
			box = cyanStyle.Render(box)
			name = cyanStyle.Render(name)
		} else if !m.picker.checked[i] {
			name = dimStyle.Render(name)
		}
		row := marker + box + " " + name
		if m.picker.sessions[i] > 0 {
			row += dimStyle.Render(fmt.Sprintf("  (%d running)", m.picker.sessions[i]))
		}
		b.WriteString(row)
	}

	var f strings.Builder
	if hiddenRunning > 0 {
		f.WriteString(yellowStyle.Render(fmt.Sprintf("%d running session(s) will be hidden but keep running", hiddenRunning)))
		f.WriteString("\n")
	}
	if m.statusMsg != "" {
		f.WriteString(dimStyle.Render(m.statusMsg))
		f.WriteString("\n")
	}
	f.WriteString(dimStyle.Render("↑↓ move · space toggle · enter save · esc cancel"))

	return m.withBottomFooter(b.String(), f.String())
}

func boldTitle(s string) string {
	return lipgloss.NewStyle().Bold(true).Render(s)
}

// titleColWidth returns the display width needed for the title column of the
// current project, or 0 when no session has a title.
func (m model) titleColWidth() int {
	w := 0
	for _, s := range m.currentProject().Sessions {
		if n := len([]rune(s.Title)); n > w {
			w = n
		}
	}
	if w > maxTitleLen {
		w = maxTitleLen
	}
	return w
}

func (m model) renderSessionRow(i, titleWidth int) string {
	p := m.currentProject()
	s := p.Sessions[i]
	selected := i == m.row

	marker := "  "
	if selected {
		marker = cyanStyle.Render("❯ ")
	}

	num := fmt.Sprintf("%d", s.Slot)
	if selected {
		num = cyanStyle.Render(num)
	} else {
		num = dimStyle.Render(num)
	}

	row := marker + num

	if titleWidth > 0 {
		title := []rune(s.Title)
		if len(title) > titleWidth {
			title = append(title[:titleWidth-1], '…')
		}
		text := string(title) + strings.Repeat(" ", titleWidth-len(title))
		if selected {
			text = cyanStyle.Render(text)
		} else if s.Title == "" {
			text = dimStyle.Render(text)
		}
		row += "  " + text
	}

	row += "  " + statusLabel(s.Status)

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
