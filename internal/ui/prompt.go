package ui

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"golang.org/x/term"
)

// ErrNoTerminal is returned by a prompt that has no terminal to ask on.
var ErrNoTerminal = errors.New("no terminal to ask on")

// Back is what Choose returns when the operator backs out (Esc / Ctrl-C), and
// the option the menus offer for it.
const Back = "Back"

// Prompter asks the operator. Every method backs out cleanly: Esc or Ctrl-C in
// Choose returns Back, in Confirm returns false, in Path returns "".
type Prompter interface {
	Choose(title string, options ...string) (string, error)
	Confirm(title string) (bool, error)
	Input(title, placeholder string) (string, error)
	// Path asks for a directory with Tab completion, starting from prefill.
	// It returns the cleaned absolute path, or "" when left empty.
	Path(title, prefill string) (string, error)
	// Pager shows content scrollably; without a terminal it prints it.
	Pager(content string) error
	Interactive() bool
}

// TTY prompts on the terminal (huh forms, a bubbles viewport pager).
type TTY struct {
	In  *os.File
	Out io.Writer
}

// NewTTY returns a terminal prompter on stdin/stderr.
func NewTTY() *TTY { return &TTY{In: os.Stdin, Out: os.Stderr} }

// Interactive reports whether stdin is a terminal.
func (t *TTY) Interactive() bool { return term.IsTerminal(int(t.In.Fd())) }

func (t *TTY) run(f huh.Field) error {
	if !t.Interactive() {
		return ErrNoTerminal
	}
	return huh.NewForm(huh.NewGroup(f)).WithInput(t.In).WithOutput(t.Out).WithShowHelp(false).Run()
}

// Choose shows a selection list.
func (t *TTY) Choose(title string, options ...string) (string, error) {
	var v string
	opts := make([]huh.Option[string], len(options))
	for i, o := range options {
		opts[i] = huh.NewOption(o, o)
	}
	err := t.run(huh.NewSelect[string]().Title(title).Options(opts...).Value(&v))
	if errors.Is(err, huh.ErrUserAborted) {
		return Back, nil
	}
	return v, err
}

// Confirm asks yes/no; No is the default.
func (t *TTY) Confirm(title string) (bool, error) {
	var v bool
	err := t.run(huh.NewConfirm().Title(title).Affirmative("Yes").Negative("No").Value(&v))
	if errors.Is(err, huh.ErrUserAborted) {
		return false, nil
	}
	return v, err
}

// Input asks for one line.
func (t *TTY) Input(title, placeholder string) (string, error) {
	var v string
	err := t.run(huh.NewInput().Title(title).Placeholder(placeholder).Value(&v))
	if errors.Is(err, huh.ErrUserAborted) {
		return "", nil
	}
	return strings.TrimSpace(v), err
}

// Path asks for a directory, completing entries on Tab.
func (t *TTY) Path(title, prefill string) (string, error) {
	v := prefill
	in := huh.NewInput().Title(title).Value(&v)
	in.SuggestionsFunc(func() []string { return PathSuggestions(v) }, &v)
	err := t.run(in)
	if errors.Is(err, huh.ErrUserAborted) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return CleanPath(v), nil
}

// PathSuggestions completes the last element of a typed path to the
// directories next to it.
func PathSuggestions(typed string) []string {
	expanded := expandHome(typed)
	dir, prefix := filepath.Split(expanded)
	if dir == "" {
		dir = "."
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), prefix) || (strings.HasPrefix(e.Name(), ".") && !strings.HasPrefix(prefix, ".")) {
			continue
		}
		// suggestions must extend what was typed, so keep its spelling (~ included)
		out = append(out, typed[:len(typed)-len(prefix)]+e.Name()+"/")
	}
	sort.Strings(out)
	return out
}

func expandHome(p string) string {
	home, _ := os.UserHomeDir()
	switch {
	case p == "~":
		return home
	case strings.HasPrefix(p, "~/"):
		return home + p[1:]
	}
	return p
}

// CleanPath is ask_dir's normalisation: ~ expanded, a trailing / dropped, a
// relative path made absolute, symlinks resolved when the path exists.
func CleanPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = expandHome(p)
	if len(p) > 1 {
		p = strings.TrimRight(p, "/")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

// Pager shows content in a scrollable viewport (q, Esc or Ctrl-C to leave);
// without a terminal it prints content as is.
func (t *TTY) Pager(content string) error {
	if !t.Interactive() || !IsTerminal(t.Out) {
		_, err := io.WriteString(t.Out, content)
		return err
	}
	_, err := tea.NewProgram(pager{content: content}, tea.WithInput(t.In), tea.WithOutput(t.Out), tea.WithAltScreen()).Run()
	return err
}

type pager struct {
	content string
	vp      viewport.Model
	ready   bool
}

func (p pager) Init() tea.Cmd { return nil }

func (p pager) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case tea.KeyMsg:
		switch m.String() {
		case "q", "esc", "ctrl+c":
			return p, tea.Quit
		}
	case tea.WindowSizeMsg:
		if !p.ready {
			p.vp = viewport.New(m.Width, m.Height-1)
			p.vp.SetContent(p.content)
			p.ready = true
		} else {
			p.vp.Width, p.vp.Height = m.Width, m.Height-1
		}
	}
	var cmd tea.Cmd
	p.vp, cmd = p.vp.Update(msg)
	return p, cmd
}

func (p pager) View() string {
	if !p.ready {
		return ""
	}
	return p.vp.View() + fmt.Sprintf("\n %3.f%%  ↑/↓ PgUp/PgDn scroll, q quits", p.vp.ScrollPercent()*100)
}

// Script is a Prompter for tests: it answers from a queue and records each question.
type Script struct {
	Answers []string // Choose: the option; Confirm: "yes"/"no"; Input/Path: the text
	Asked   []string
	Paged   []string
}

func (s *Script) next(q string) (string, error) {
	s.Asked = append(s.Asked, q)
	if len(s.Answers) == 0 {
		return "", fmt.Errorf("script: no answer for %q", q)
	}
	a := s.Answers[0]
	s.Answers = s.Answers[1:]
	return a, nil
}

// Choose answers from the script; the answer must be one of options.
func (s *Script) Choose(title string, options ...string) (string, error) {
	a, err := s.next("CHOOSE " + title + " [" + strings.Join(options, "|") + "]")
	if err != nil {
		return "", err
	}
	for _, o := range options {
		if o == a {
			return a, nil
		}
	}
	return "", fmt.Errorf("script: %q is not an option of %v", a, options)
}

// Confirm answers from the script.
func (s *Script) Confirm(title string) (bool, error) {
	a, err := s.next("CONFIRM " + title)
	return a == "yes", err
}

// Input answers from the script.
func (s *Script) Input(title, _ string) (string, error) { return s.next("INPUT " + title) }

// Path answers from the script, cleaned like the terminal prompt.
func (s *Script) Path(title, _ string) (string, error) {
	a, err := s.next("PATH " + title)
	return CleanPath(a), err
}

// Pager records the content.
func (s *Script) Pager(content string) error {
	s.Paged = append(s.Paged, content)
	return nil
}

// Interactive is always true for a script.
func (s *Script) Interactive() bool { return true }
