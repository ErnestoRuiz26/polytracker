package main

// ui.go — Minimal stdout progress feedback for the slow startup phases.
// Operational logs are quiet by default (log_level=warn), so without this the
// program looks frozen while it fans out hundreds of API calls. The spinner
// writes to stdout (not the gated logger) so it shows regardless of log level.

import (
	"fmt"
	"os"
	"time"
)

// isTerminal reports whether f is an interactive character device. When stdout
// is redirected to a file or pipe, animation would just spew control codes, so
// the spinner degrades to a single static line instead.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// spinner shows an animated "working…" line on a TTY, or a one-shot message
// when stdout is not interactive.
type spinner struct {
	stop chan struct{}
	done chan struct{}
	msg  string
	tty  bool
}

// startSpinner begins showing msg. Call Stop to end it.
func startSpinner(msg string) *spinner {
	s := &spinner{
		stop: make(chan struct{}),
		done: make(chan struct{}),
		msg:  msg,
		tty:  isTerminal(os.Stdout),
	}
	if !s.tty {
		// Non-interactive: print once and we're done.
		fmt.Printf("%s\n", msg)
		close(s.done)
		return s
	}
	go s.run()
	return s
}

func (s *spinner) run() {
	frames := []rune{'⠋', '⠙', '⠹', '⠸', '⠼', '⠴', '⠦', '⠧', '⠇', '⠏'}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	i := 0
	for {
		select {
		case <-s.stop:
			close(s.done)
			return
		case <-ticker.C:
			fmt.Printf("\r%c %s", frames[i%len(frames)], s.msg)
			i++
		}
	}
}

// Stop halts the spinner. If final is non-empty it is printed as the resting
// line (e.g. a completion summary); the animated line is cleared first on a TTY.
func (s *spinner) Stop(final string) {
	if s.tty {
		close(s.stop)
		<-s.done
		fmt.Print("\r\033[K") // carriage return + clear-to-end-of-line
	} else {
		<-s.done
	}
	if final != "" {
		fmt.Printf("%s\n", final)
	}
}
