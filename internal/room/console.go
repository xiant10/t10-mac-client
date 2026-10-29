package room

import (
	"fmt"
	"io"
	"sync"
)

// console serializes everything this client prints for one exchange: the
// user's turn (typed text, or the transcript of a voice message) is always
// one complete line printed via line(); the agent's reply streams in one
// rune at a time via agentChunk and is closed off with agentEnd. A mutex is
// enough to keep them from tearing across goroutines — unlike the old
// live-call client, at most one exchange is ever in flight (see cmd/client's
// REPL), so there's no "whoever is talking owns the line" ownership problem
// to solve, just "don't interleave two concurrent writers."
type console struct {
	mu       sync.Mutex
	w        io.Writer
	replying bool
}

func newConsole(w io.Writer) *console { return &console{w: w} }

// line prints one complete line, first closing out any agent reply still in
// progress (defensive: the agent's reply topics fire from separate
// goroutines per the SDK, so could in principle interleave with this).
func (c *console) line(text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.endAgentLocked()
	fmt.Fprintln(c.w, text)
}

// agentChunk appends one piece of the agent's streamed reply, printing the
// "Agent: " prefix the first time it's called for a given reply.
func (c *console) agentChunk(text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.replying {
		fmt.Fprint(c.w, "Agent: ")
		c.replying = true
	}
	fmt.Fprint(c.w, text)
}

// agentEnd closes out the agent's reply line, if one is open. A no-op if
// the reply was empty.
func (c *console) agentEnd() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.endAgentLocked()
}

func (c *console) endAgentLocked() {
	if !c.replying {
		return
	}
	fmt.Fprint(c.w, "\n\n")
	c.replying = false
}
