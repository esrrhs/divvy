package agent

import (
	"context"
	"fmt"
)

// channelGuided is the non-terminal GuidedIO: every user action arrives as a
// command line through Send (the web layer posts one line per action), and
// Guider's status prints are forwarded to onPrint instead of a TTY.
//
// The channel is never closed by the manager: the web equivalent of terminal
// EOF is an explicit abort/pause, and closing while a Send raced the run
// shutdown would panic on send. Handles become garbage once the run ends.
type channelGuided struct {
	ch      chan string
	onPrint func(string)
}

// guidedLineBuffer bounds command lines queued while the guider is briefly
// busy inside decomposition or root acceptance (not selecting on Lines).
const guidedLineBuffer = 32

func newChannelGuided(onPrint func(string)) *channelGuided {
	return &channelGuided{ch: make(chan string, guidedLineBuffer), onPrint: onPrint}
}

func (g *channelGuided) Lines() <-chan string { return g.ch }

func (g *channelGuided) Printf(format string, a ...any) {
	if g.onPrint != nil {
		g.onPrint(fmt.Sprintf(format, a...))
	}
}

// Submit delivers one command line, unblocking when the run shuts down
// before the line can be consumed.
func (g *channelGuided) Submit(ctx context.Context, line string) error {
	select {
	case g.ch <- line:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
