package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// errNoTerminal means resume was run without a terminal on stdin: from a
// script, a pipe, a scheduler, or a coding agent's shell. A halt is lifted
// only by a person at a terminal, so there is no flag to skip the check.
var errNoTerminal = errors.New("resume needs a terminal on stdin: a person at a terminal lifts a halt, not a script")

// errNotConfirmed means the halt's ID was not typed back.
var errNotConfirmed = errors.New("not confirmed")

// terminalConfirm asks the person at the terminal on in to type the
// halt's ID, prompting on prompt. It is a speed bump against automation,
// not authentication: a process that allocates a pseudo-terminal passes
// it, and anyone with the database URL can clear a halt directly. The
// resume alert is what makes a resume the owner did not run visible.
func terminalConfirm(in *os.File, prompt io.Writer) func(haltID int64) error {
	return func(haltID int64) error {
		if !isTerminal(in) {
			return errNoTerminal
		}
		return confirmID(in, prompt, haltID)
	}
}

// isTerminal reports whether f is a character device. A pipe, a regular
// file or a closed stdin is not; /dev/null is, but gives an empty line,
// which confirmID refuses.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// confirmID prompts for haltID and reads one line from in, which must be
// exactly the ID.
func confirmID(in io.Reader, prompt io.Writer, haltID int64) error {
	want := strconv.FormatInt(haltID, 10)
	_, _ = fmt.Fprintf(prompt, "Type %s to lift halt %s: ", want, want)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("reading the confirmation: %w", err)
	}
	if strings.TrimSpace(line) != want {
		return errNotConfirmed
	}
	return nil
}
