// Package ui is how captain_utils talks to the operator. Data (tables, CSV,
// JSON, diffs) goes to Out; status lines go to Err, so stdout stays pipeable.
// Colour is on only for a terminal, and never with --no-color or NO_COLOR.
//
// Interactive prompts (the menu, confirms, the path prompt) join this package
// with the helm flows; everything here works without a terminal.
package ui

import (
	"fmt"
	"io"
	"os"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
	"github.com/muesli/termenv"
	"golang.org/x/term"
)

// Printer writes styled output.
type Printer struct {
	Out, Err io.Writer
	out, err *lipgloss.Renderer
}

// IsTerminal reports whether w is a terminal.
func IsTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// New builds a Printer. noColor forces plain output.
func New(out, err io.Writer, noColor bool) *Printer {
	if _, set := os.LookupEnv("NO_COLOR"); set {
		noColor = true
	}
	mk := func(w io.Writer) *lipgloss.Renderer {
		r := lipgloss.NewRenderer(w)
		if noColor || !IsTerminal(w) {
			r.SetColorProfile(termenv.Ascii)
		}
		return r
	}
	return &Printer{Out: out, Err: err, out: mk(out), err: mk(err)}
}

// Colours used by the old gum menu, kept so the output reads the same.
const (
	colorPink   = "212"
	colorRed    = "196"
	colorOrange = "214"
	colorGrey   = "245"
	colorGreen  = "46"
)

func (p *Printer) line(color string, bold bool, format string, a ...any) {
	s := p.err.NewStyle().Foreground(lipgloss.Color(color)).Bold(bold)
	fmt.Fprintln(p.Err, s.Render(fmt.Sprintf(format, a...)))
}

// Info is a progress line.
func (p *Printer) Info(format string, a ...any) { p.line(colorPink, false, format, a...) }

// Note is a low-key line.
func (p *Printer) Note(format string, a ...any) { p.line(colorGrey, false, format, a...) }

// Success is a ✅ line.
func (p *Printer) Success(format string, a ...any) { p.line(colorGreen, false, "✅ "+format, a...) }

// Warn is an orange line.
func (p *Printer) Warn(format string, a ...any) { p.line(colorOrange, false, format, a...) }

// Error is a red ❌ line.
func (p *Printer) Error(format string, a ...any) { p.line(colorRed, false, "❌ "+format, a...) }

// Highlight is a bold pink line (gum style --bold --foreground 212).
func (p *Printer) Highlight(format string, a ...any) { p.line(colorPink, true, format, a...) }

// Alert is a bold red line without the ❌ (gum style --bold --foreground 196).
func (p *Printer) Alert(format string, a ...any) { p.line(colorRed, true, format, a...) }

// Plain is an unstyled status line.
func (p *Printer) Plain(format string, a ...any) { fmt.Fprintf(p.Err, format+"\n", a...) }

// Table renders a bordered table to Out, styled like the gum table it replaces.
func (p *Printer) Table(headers []string, rows [][]string) {
	r := p.out
	t := table.New().
		Border(lipgloss.NormalBorder()).
		BorderStyle(r.NewStyle().Foreground(lipgloss.Color("63"))).
		Headers(headers...).
		Rows(rows...).
		StyleFunc(func(row, _ int) lipgloss.Style {
			s := r.NewStyle().Padding(0, 1).Align(lipgloss.Center)
			if row == table.HeaderRow {
				return s.Foreground(lipgloss.Color("#FFAA00")).Bold(true)
			}
			return s
		})
	fmt.Fprintln(p.Out, t.Render())
}
