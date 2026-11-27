package ui

import "fmt"

// Confirm asks title. Only yes (the --yes flag) answers for the operator. Without a terminal
// and without --yes it is a usage error (exit 2: the command line has to say yes), and a no
// (or Esc) is ErrDeclined, so a declined run is not mistaken for an applied one.
func Confirm(pr Prompter, p *Printer, yes bool, title string) error {
	if yes {
		p.Note("%s yes (--yes)", title)
		return nil
	}
	if !pr.Interactive() {
		return Usage(fmt.Errorf("%s: %w (pass --yes to run without one)", title, ErrNoTerminal))
	}
	ok, err := pr.Confirm(title)
	if err != nil {
		return err
	}
	if !ok {
		return ErrDeclined
	}
	return nil
}
