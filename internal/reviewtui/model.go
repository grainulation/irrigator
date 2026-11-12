package reviewtui

import (
	"fmt"
	"io"
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/grainulation/irrigator/internal/textsafe"
)

// Item is one review branch in the queue.
type Item struct {
	Branch string
	Head   string
	Ahead  int
	Files  int
	Latest string
	// Status is the human verdict state: pending, approved, changes requested
	// or awaiting agent.
	Status string
	// Overlap is true when a file in this change is also changed on another
	// review branch.
	Overlap bool
	Current bool
}

// Backend supplies queue data and records decisions. Head arguments pin the
// reviewed commit: implementations must refuse when the branch has moved, so a
// verdict never lands on a head the reviewer did not see.
type Backend interface {
	Queue() (base string, items []Item, err error)
	Diff(branch, head string) (string, error)
	Approve(branch, head string) error
	RequestChanges(branch, head, note string) error
}

// options configure the model.
type options struct {
	noColor bool
}

// maxCopyBytes caps what one copy puts on the clipboard. A branch name or a
// diff command is far smaller; the cap only guards pathological names.
const maxCopyBytes = 1024

// diffCommand returns a ready-to-run command that shows a branch's changes
// against base, quoting arguments so a branch name cannot inject shell syntax.
func diffCommand(base, branch string) string {
	return "git diff " + shellQuote(base) + "..." + shellQuote(branch)
}

// shellQuote leaves plain words alone and single-quotes anything else.
func shellQuote(s string) string {
	plain := s != ""
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./@+:=", r)) {
			plain = false
			break
		}
	}
	if plain && !strings.HasPrefix(s, "-") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

type pane int

const (
	paneList pane = iota
	paneDiff
)

type queueMsg struct {
	base  string
	items []Item
	err   error
}

type diffMsg struct {
	branch, head, text string
	err                error
}

type decisionMsg struct {
	branch, verdict string
	err             error
}

type keyMap struct {
	Move, Switch, Jump, Approve, Request, Refresh, Copy, CopyCmd, Help, Quit key.Binding
}

func newKeyMap() keyMap {
	return keyMap{
		Move:    key.NewBinding(key.WithKeys("j", "k", "up", "down"), key.WithHelp("j/k", "move")),
		Switch:  key.NewBinding(key.WithKeys("enter", "tab"), key.WithHelp("enter/tab", "pane")),
		Jump:    key.NewBinding(key.WithKeys("n", "p"), key.WithHelp("n/p", "hunk")),
		Approve: key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "approve")),
		Request: key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "request changes")),
		Refresh: key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "refresh")),
		Copy:    key.NewBinding(key.WithKeys("y"), key.WithHelp("y", "copy branch")),
		CopyCmd: key.NewBinding(key.WithKeys("Y"), key.WithHelp("Y", "copy diff command")),
		Help:    key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Quit:    key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
	}
}

func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Move, k.Switch, k.Jump, k.Approve, k.Request, k.Copy, k.CopyCmd, k.Help, k.Quit}
}
func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{{k.Move, k.Switch, k.Jump}, {k.Approve, k.Request, k.Refresh}, {k.Copy, k.CopyCmd}, {k.Help, k.Quit}}
}

// model is the Bubble Tea model.
type model struct {
	backend Backend
	color   bool
	keys    keyMap
	help    help.Model
	list    list.Model
	diff    viewport.Model
	note    textinput.Model

	width, height int
	focus         pane
	base          string
	items         []Item
	loaded        bool
	prompting     bool
	showHelp      bool
	status        string
	shown         string // branch@head whose diff is displayed or requested
	diffLines     []string
	marks         []int // line indexes of file headers and hunks
	mark          int   // index into marks of the last n/p stop, -1 before the first
	diffErr       string
}

// newModel returns a model that loads the queue from backend on Init.
func newModel(backend Backend, opts options) model {
	l := list.New(nil, itemDelegate{color: !opts.noColor}, 0, 0)
	l.SetShowTitle(false)
	l.SetShowStatusBar(false)
	l.SetShowHelp(false)
	l.SetShowFilter(false)
	l.SetShowPagination(false)
	l.SetFilteringEnabled(false)
	l.DisableQuitKeybindings()
	ti := textinput.New()
	ti.Prompt = "note> "
	ti.CharLimit = 500
	h := help.New()
	if opts.noColor {
		plain := lipgloss.NewStyle()
		h.Styles = help.Styles{Ellipsis: plain, ShortKey: plain, ShortDesc: plain, ShortSeparator: plain, FullKey: plain, FullDesc: plain, FullSeparator: plain}
		ts := textinput.DefaultStyles(false)
		ts.Focused = textinput.StyleState{Text: plain, Placeholder: plain, Suggestion: plain, Prompt: plain}
		ts.Blurred = ts.Focused
		ts.Cursor.Color = nil
		ti.SetStyles(ts)
	}
	return model{
		backend: backend, color: !opts.noColor, keys: newKeyMap(), help: h,
		list: l, diff: viewport.New(), note: ti, width: 80, height: 24, mark: -1,
	}
}

// Init loads the review queue.
func (m model) Init() tea.Cmd { return m.loadQueue() }

func (m model) loadQueue() tea.Cmd {
	return func() tea.Msg {
		base, items, err := m.backend.Queue()
		return queueMsg{base: base, items: items, err: err}
	}
}

func (m model) loadDiff(it Item) tea.Cmd {
	return func() tea.Msg {
		text, err := m.backend.Diff(it.Branch, it.Head)
		return diffMsg{branch: it.Branch, head: it.Head, text: text, err: err}
	}
}

func (m model) selected() (Item, bool) {
	it, ok := m.list.SelectedItem().(listItem)
	return it.Item, ok
}

func key1(it Item) string { return it.Branch + "@" + it.Head }

// Update handles messages.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		return m, nil
	case queueMsg:
		return m.onQueue(msg)
	case diffMsg:
		if it, ok := m.selected(); !ok || key1(it) != msg.branch+"@"+msg.head {
			return m, nil // stale reply for a branch no longer selected
		}
		m.diffErr = ""
		if msg.err != nil {
			m.diffErr = msg.err.Error()
			msg.text = ""
		}
		m.setDiff(msg.text)
		return m, nil
	case decisionMsg:
		if msg.err != nil {
			m.status = "error: " + msg.err.Error()
			return m, nil
		}
		m.status = msg.verdict + " " + msg.branch
		return m, m.loadQueue()
	case tea.KeyPressMsg:
		return m.onKey(msg)
	}
	return m, nil
}

func (m model) onQueue(msg queueMsg) (tea.Model, tea.Cmd) {
	m.loaded = true
	if msg.err != nil {
		m.status = "error: " + msg.err.Error()
		return m, nil
	}
	prev, had := m.selected()
	m.base, m.items = msg.base, msg.items
	li := make([]list.Item, len(msg.items))
	idx := 0
	for i, it := range msg.items {
		li[i] = listItem{it}
		if had && it.Branch == prev.Branch {
			idx = i
		}
	}
	m.list.SetItems(li)
	m.layout()
	if len(msg.items) == 0 {
		m.shown = ""
		m.setDiff("")
		return m, nil
	}
	m.list.Select(idx)
	it, _ := m.selected()
	if key1(it) == m.shown && m.diffErr == "" {
		return m, nil
	}
	m.shown = key1(it)
	m.setDiff("")
	return m, m.loadDiff(it)
}

func (m model) onKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return m, tea.Quit
	}
	if m.prompting {
		return m.onPromptKey(msg)
	}
	switch {
	case key.Matches(msg, m.keys.Quit):
		return m, tea.Quit
	case key.Matches(msg, m.keys.Help):
		m.showHelp = !m.showHelp
		m.help.ShowAll = m.showHelp
		m.layout()
		return m, nil
	case key.Matches(msg, m.keys.Switch):
		m.focus = 1 - m.focus
		return m, nil
	case key.Matches(msg, m.keys.Refresh):
		m.status = "refreshing"
		return m, m.loadQueue()
	case key.Matches(msg, m.keys.Jump):
		m.jump(msg.String() == "n")
		return m, nil
	case key.Matches(msg, m.keys.Copy), key.Matches(msg, m.keys.CopyCmd):
		it, ok := m.selected()
		if !ok {
			return m, nil
		}
		text := it.Branch
		if key.Matches(msg, m.keys.CopyCmd) {
			text = diffCommand(m.base, it.Branch)
		}
		if len(text) > maxCopyBytes {
			m.status = "not copied: longer than " + fmt.Sprint(maxCopyBytes) + " bytes"
			return m, nil
		}
		if strings.ContainsAny(text, "\r\n") {
			m.status = "not copied: contains newline"
			return m, nil
		}
		// OSC 52 through the terminal: works over ssh and inside tmux/herdr
		// panes wherever the terminal allows clipboard writes.
		m.status = "copied"
		return m, tea.SetClipboard(text)
	case key.Matches(msg, m.keys.Approve):
		it, ok := m.selected()
		if !ok {
			return m, nil
		}
		m.status = "approving " + it.Branch
		return m, func() tea.Msg {
			return decisionMsg{branch: it.Branch, verdict: "approved", err: m.backend.Approve(it.Branch, it.Head)}
		}
	case key.Matches(msg, m.keys.Request):
		if _, ok := m.selected(); !ok {
			return m, nil
		}
		m.prompting = true
		m.note.Reset()
		m.note.Focus() // blink command intentionally dropped: a steady cursor
		m.status = ""
		return m, nil
	}
	if m.focus == paneDiff {
		var cmd tea.Cmd
		m.diff, cmd = m.diff.Update(msg)
		return m, cmd
	}
	before, _ := m.selected()
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	if now, ok := m.selected(); ok && key1(now) != key1(before) {
		m.shown = key1(now)
		m.setDiff("")
		return m, tea.Batch(cmd, m.loadDiff(now))
	}
	return m, cmd
}

func (m model) onPromptKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.prompting = false
		m.note.Blur()
		m.status = "request cancelled"
		return m, nil
	case "enter":
		text := strings.TrimSpace(m.note.Value())
		if text == "" {
			m.status = "a note is required (esc cancels)"
			return m, nil
		}
		it, _ := m.selected()
		m.prompting = false
		m.note.Blur()
		m.status = "requesting changes on " + it.Branch
		return m, func() tea.Msg {
			return decisionMsg{branch: it.Branch, verdict: "changes requested on", err: m.backend.RequestChanges(it.Branch, it.Head, text)}
		}
	}
	var cmd tea.Cmd
	m.note, cmd = m.note.Update(msg)
	return m, cmd
}

// jump moves the diff to the next or previous file header or hunk.
func (m *model) jump(forward bool) {
	if len(m.marks) == 0 {
		return
	}
	if forward {
		m.mark = min(m.mark+1, len(m.marks)-1)
	} else {
		m.mark = max(m.mark-1, 0)
	}
	m.diff.SetYOffset(m.marks[m.mark])
}

func (m *model) setDiff(text string) {
	m.diffLines, m.marks, m.mark = nil, nil, -1
	if text != "" {
		m.diffLines = diffDisplayLines(text)
		for i, l := range m.diffLines {
			if strings.HasPrefix(l, "diff --git ") || strings.HasPrefix(l, "@@") {
				m.marks = append(m.marks, i)
			}
		}
	}
	m.renderDiff()
	m.diff.GotoTop()
}

// diffDisplayLines turns raw diff text into sanitized display lines. It is the
// only place diff text crosses into presentation, and it never changes the
// caller's raw text. A CR that terminates a line (CRLF endings) is the line
// ending, not content, so it is dropped before sanitizing; a CR anywhere else,
// including one on the final unterminated line, stays and is shown as \r.
// Trailing empty lines are dropped.
func diffDisplayLines(raw string) []string {
	lines := strings.Split(raw, "\n")
	for i := 0; i < len(lines)-1; i++ { // the last piece has no LF after it
		lines[i] = strings.TrimSuffix(lines[i], "\r")
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for i, l := range lines {
		lines[i] = textsafe.Terminal(l)
	}
	return lines
}

func (m *model) renderDiff() {
	out := make([]string, len(m.diffLines))
	for i, l := range m.diffLines {
		out[i] = m.styleDiffLine(l)
	}
	m.diff.SetContent(strings.Join(out, "\n"))
}

func (m model) styleDiffLine(l string) string {
	if !m.color {
		return l
	}
	s := lipgloss.NewStyle()
	switch {
	case strings.HasPrefix(l, "diff --git "), strings.HasPrefix(l, "index "), strings.HasPrefix(l, "--- "), strings.HasPrefix(l, "+++ "):
		s = s.Bold(true)
	case strings.HasPrefix(l, "@@"):
		s = s.Foreground(lipgloss.Color("6"))
	case strings.HasPrefix(l, "+"):
		s = s.Foreground(lipgloss.Color("2"))
	case strings.HasPrefix(l, "-"):
		s = s.Foreground(lipgloss.Color("1"))
	default:
		return l
	}
	return s.Render(l)
}

func (m model) leftWidth() int {
	w := m.width / 3
	if w < 24 {
		w = 24
	}
	if w > 44 {
		w = 44
	}
	if w > m.width-10 {
		w = max(m.width-10, 1)
	}
	return w
}

func (m *model) layout() {
	m.help.SetWidth(m.width)
	lw := m.leftWidth()
	bodyH := max(m.height-m.footerHeight()-1, 1)
	m.list.SetSize(lw, bodyH)
	m.diff.SetWidth(max(m.width-lw-3, 1))
	m.diff.SetHeight(bodyH)
	m.note.SetWidth(max(m.width-10, 10))
}

func (m model) footerHeight() int {
	if m.showHelp {
		return 2 + strings.Count(m.help.FullHelpView(m.keys.FullHelp()), "\n") + 1
	}
	return 2
}

func (m model) paint(s string, st lipgloss.Style) string {
	if !m.color {
		return s
	}
	return st.Render(s)
}

// View renders the two panes, status line and help.
func (m model) View() tea.View {
	lw := m.leftWidth()
	lt, rt := "Review branches", "Diff"
	if it, ok := m.selected(); ok {
		rt = "Diff: " + textsafe.Terminal(it.Branch)
	}
	if m.base != "" {
		lt += " (base " + textsafe.Terminal(m.base) + ")"
	}
	active := lipgloss.NewStyle().Bold(true).Reverse(true)
	idle := lipgloss.NewStyle().Faint(true)
	title := func(s string, w int, on bool) string {
		s = ansiTrunc(s, w)
		if on {
			if !m.color {
				return "[" + s + "]"
			}
			return active.Render(s)
		}
		return m.paint(s, idle)
	}
	left := m.leftBody(lw)
	right := m.rightBody()
	cols := lipgloss.JoinHorizontal(lipgloss.Top,
		lipgloss.NewStyle().Width(lw).Render(title(lt, lw, m.focus == paneList)+"\n"+left),
		" │ ",
		title(rt, max(m.width-lw-3, 1), m.focus == paneDiff)+"\n"+right,
	)
	var foot string
	switch {
	case m.prompting:
		foot = "Request changes — " + m.note.View() + "  (enter sends, esc cancels) " + textsafe.Terminal(m.status)
	default:
		foot = textsafe.Terminal(m.status)
	}
	helpText := m.help.ShortHelpView(m.keys.ShortHelp())
	if m.showHelp {
		helpText = m.help.FullHelpView(m.keys.FullHelp())
	}
	v := tea.NewView(cols + "\n" + foot + "\n" + helpText)
	v.AltScreen = true
	return v
}

func (m model) leftBody(w int) string {
	switch {
	case !m.loaded:
		return "loading…"
	case len(m.items) == 0:
		return "No branches are ahead of the base."
	}
	body := m.list.View()
	if m.hasOverlap() {
		body += "\n* changed on more than one branch"
	}
	return body
}

func (m model) hasOverlap() bool {
	for _, it := range m.items {
		if it.Overlap {
			return true
		}
	}
	return false
}

func (m model) rightBody() string {
	switch {
	case m.diffErr != "":
		return "error: " + textsafe.Terminal(m.diffErr)
	case len(m.items) == 0:
		return ""
	case len(m.diffLines) == 0:
		return "loading…"
	}
	return m.diff.View()
}

func ansiTrunc(s string, w int) string {
	if w <= 0 {
		return ""
	}
	return lipgloss.NewStyle().MaxWidth(w).Render(s)
}

// --- list plumbing -------------------------------------------------------------

type listItem struct{ Item }

func (i listItem) FilterValue() string { return i.Branch }

type itemDelegate struct{ color bool }

func (itemDelegate) Height() int                         { return 2 }
func (itemDelegate) Spacing() int                        { return 0 }
func (itemDelegate) Update(tea.Msg, *list.Model) tea.Cmd { return nil }
func (d itemDelegate) Render(w io.Writer, m list.Model, index int, li list.Item) {
	it, ok := li.(listItem)
	if !ok {
		return
	}
	mark, cur := " ", ""
	if it.Overlap {
		mark = "*"
	}
	if it.Current {
		cur = " (checked out)"
	}
	sel := index == m.Index()
	prefix := "  "
	if sel {
		prefix = "> "
	}
	width := m.Width()
	line1 := ansiTrunc(fmt.Sprintf("%s%s %s%s", prefix, mark, textsafe.Terminal(it.Branch), cur), width)
	line2 := ansiTrunc(fmt.Sprintf("    %s · %d file(s) · %d ahead", textsafe.Terminal(it.Status), it.Files, it.Ahead), width)
	if d.color {
		st := lipgloss.NewStyle()
		switch it.Status {
		case "approved":
			line2 = st.Foreground(lipgloss.Color("2")).Render(line2)
		case "changes requested", "awaiting agent":
			line2 = st.Foreground(lipgloss.Color("3")).Render(line2)
		default:
			line2 = st.Faint(true).Render(line2)
		}
		if sel {
			line1 = st.Bold(true).Render(line1)
		}
	}
	fmt.Fprintf(w, "%s\n%s", line1, line2)
}
