package spinner

import (
	"fmt"
	"io"
	"sync"
	"time"

	lipgloss "charm.land/lipgloss/v2"

	"github.com/mo-cmyk/mazarbul/internal/tui/style"
)

// brailleFrames are the eight-phase braille rotation frames used by Inline.
var brailleFrames = []string{"⣾", "⣽", "⣻", "⢿", "⡿", "⣟", "⣯", "⣷"}

// Inline is a plain-terminal (non-bubbletea) spinner that writes animated
// progress frames directly to a writer while work is in progress. It is safe
// for use outside a Bubble Tea program — for example in CLI commands that run
// a single blocking operation.
//
// When enabled is false, no frame animation or cursor-control escapes are
// emitted; Start becomes a no-op and Done writes a plain "✓ msg\n" line.
// Callers should pass enabled = (tty && !NO_COLOR).
type Inline struct {
	w        io.Writer
	label    string
	enabled  bool
	frames   []string
	stop     chan struct{}
	done     chan struct{}
	mu       sync.Mutex
	started  bool
	stopOnce sync.Once
}

// NewInline returns an Inline spinner writing to w.
// Label is the activity description shown beside each frame.
// Enabled should be true when the terminal supports color/TTY output.
func NewInline(w io.Writer, label string, enabled bool) *Inline {
	return &Inline{
		w:       w,
		label:   label,
		enabled: enabled,
		frames:  brailleFrames,
	}
}

// Start begins the frame animation. When not enabled, Start is a no-op.
// Calling Start more than once has no additional effect.
//
// The goroutine waits 250 ms before showing the first frame so that fast
// operations complete silently.
func (s *Inline) Start() {
	if !s.enabled {
		return
	}
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.stop = make(chan struct{})
	s.done = make(chan struct{})
	s.mu.Unlock()

	go func() {
		defer close(s.done)

		// 250 ms initial delay — let fast operations finish before we draw.
		select {
		case <-s.stop:
			return
		case <-time.After(250 * time.Millisecond):
		}

		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()

		frame := 0
		for {
			f := s.frames[frame%len(s.frames)]
			frame++
			accent := lipgloss.NewStyle().Foreground(style.Accent()).Render(f)
			// \r moves to column 0; \033[K erases to end of line.
			fmt.Fprintf(s.w, "\r\033[K%s %s", accent, s.label) //nolint:errcheck

			select {
			case <-s.stop:
				return
			case <-ticker.C:
			}
		}
	}()
}

// Done stops the spinner and prints a completion message.
//
// When enabled: clears the spinner line (\r\033[K) then prints "✓ msg\n"
// with the check mark in ANSI green (style.ANSISuccess).
//
// When not enabled: prints "✓ msg\n" with no escape codes.
//
// Done is safe to call even if Start was never called, and idempotent — a
// second call will print the message again but will not panic or block.
func (s *Inline) Done(msg string) {
	s.mu.Lock()
	started := s.started
	s.mu.Unlock()

	if started {
		// Signal the goroutine to stop exactly once.
		s.stopOnce.Do(func() {
			close(s.stop)
		})
		// Block until the goroutine has exited and closed s.done.
		<-s.done
	}

	if s.enabled {
		// Clear whatever the spinner last wrote.
		fmt.Fprintf(s.w, "\r\033[K") //nolint:errcheck
		check := lipgloss.NewStyle().Foreground(style.ANSISuccess()).Render("✓")
		fmt.Fprintf(s.w, "%s %s\n", check, msg) //nolint:errcheck
	} else {
		fmt.Fprintf(s.w, "✓ %s\n", msg) //nolint:errcheck
	}
}
