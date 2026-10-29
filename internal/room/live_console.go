package room

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"unicode"

	"golang.org/x/term"
	"golang.org/x/text/width"
)

// Live captions occupy exactly one terminal row. Completed lines are printed
// once in full; only the bounded live preview is ever redrawn.
type liveConsole struct {
	mu                                  sync.Mutex
	w                                   io.Writer
	columns                             func() int
	owner, id, text, lastState          string
	stateAt, cutoff, finalAt, interimAt int64
	utteranceID, finalUtteranceID       uint64
	placeholder                         bool
}

func newLiveConsole(w io.Writer) *liveConsole {
	c := &liveConsole{w: w}
	c.columns = func() int {
		if f, ok := w.(*os.File); ok {
			if n, _, err := term.GetSize(int(f.Fd())); err == nil && n > 0 {
				return n
			}
		}
		return 80
	}
	return c
}

func cleanLine(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, text)
}
func runeColumns(r rune) int {
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || unicode.Is(unicode.Cf, r) {
		return 0
	}
	switch width.LookupRune(r).Kind() {
	case width.EastAsianWide, width.EastAsianFullwidth:
		return 2
	}
	return 1
}
func columnCount(text string) int {
	n := 0
	for _, r := range text {
		n += runeColumns(r)
	}
	return n
}
func previewTail(text string, columns int) string {
	if columns <= 0 {
		return ""
	}
	if columnCount(text) <= columns {
		return text
	}
	// A left-pointing mark indicates that earlier text is outside the preview.
	runes := []rune(text)
	remaining := columns - 1
	start := len(runes)
	for start > 0 {
		n := runeColumns(runes[start-1])
		if n > remaining {
			break
		}
		remaining -= n
		start--
	}
	return "‹" + string(runes[start:])
}
func (c *liveConsole) draw() {
	prefix := c.owner + ": "
	available := max(0, c.columns()-1-columnCount(prefix)) // Avoid the wrap column.
	fmt.Fprintf(c.w, "\r\x1b[2K%s%s", prefix, previewTail(c.text, available))
}
func (c *liveConsole) clear() {
	fmt.Fprint(c.w, "\r\x1b[2K")
	c.owner, c.id, c.text = "", "", ""
	c.placeholder = false
}
func (c *liveConsole) finish() {
	owner, text, placeholder := c.owner, c.text, c.placeholder
	c.clear()
	if owner != "" && text != "" && !placeholder {
		fmt.Fprintf(c.w, "%s: %s\n", owner, text)
	}
}
func (c *liveConsole) showStatus(owner, text string) {
	c.owner, c.text, c.placeholder = owner, text, true
	c.id = ""
	c.draw()
}
func (c *liveConsole) state(at int64, state string) { c.stateForUtterance(at, 0, state) }
func (c *liveConsole) stateForUtterance(at int64, utteranceID uint64, state string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if at <= c.stateAt {
		return
	}
	c.stateAt = at
	c.lastState = state
	switch state {
	case "interrupted":
		c.cutoff = max(c.cutoff, at)
		if c.owner == "Agent" {
			if !c.placeholder {
				c.text += " [interrupted]"
			}
			c.finish()
		}
	case "listening":
		if utteranceID > c.utteranceID {
			c.utteranceID = utteranceID
			if c.owner == "You" {
				c.clear()
			}
		}
		c.cutoff = max(c.cutoff, at)
		if c.owner != "You" {
			c.finish()
			c.showStatus("You", "[listening]")
		}
	case "thinking":
		// A late thinking event must not terminate and replay an active reply.
		if c.owner == "" || c.placeholder {
			c.clear()
			c.showStatus("Agent", "[thinking]")
		}
	case "idle":
		if c.placeholder {
			c.clear()
		}
	}
}
func (c *liveConsole) interim(at int64, id, text string) { c.updateInterim(at, 0, id, text, false) }
func (c *liveConsole) updateInterim(at int64, utteranceID uint64, id, text string, replace bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if utteranceID > 0 {
		if utteranceID <= c.finalUtteranceID || utteranceID < c.utteranceID {
			return
		}
	} else if at <= c.finalAt || at < c.interimAt {
		return
	}
	if c.id != id {
		// An interim is replaceable, so never commit its guess as a final line.
		if c.owner == "You" {
			c.clear()
		} else {
			c.finish()
		}
		c.owner, c.id, c.text = "You", id, ""
		c.placeholder = false
		c.interimAt = at
		c.utteranceID = utteranceID
	}
	if replace {
		c.text = cleanLine(text)
	} else {
		c.text += cleanLine(text)
	}
	c.draw()
}
func (c *liveConsole) transcript(at int64, text string) { c.finalTranscript(at, 0, text) }
func (c *liveConsole) finalTranscript(at int64, utteranceID uint64, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if utteranceID == 0 && at <= c.finalAt {
		return
	}
	c.finalAt = max(c.finalAt, at)
	c.finalUtteranceID = max(c.finalUtteranceID, utteranceID)
	owner, id, current, placeholder := c.owner, c.id, c.text, c.placeholder
	// Capture identity, not commit timestamp, tells which utterance is newer.
	preserve := owner == "Agent" || (owner == "You" && ((utteranceID > 0 && c.utteranceID > utteranceID) || (utteranceID == 0 && c.interimAt > at)))
	c.clear()
	if text != "" {
		fmt.Fprintln(c.w, "You: "+cleanLine(text))
	}
	if preserve {
		c.owner, c.id, c.text, c.placeholder = owner, id, current, placeholder
		c.draw()
	} else if c.lastState == "thinking" {
		c.showStatus("Agent", "[thinking]")
	}
}
func (c *liveConsole) reply(at int64, id, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if at <= c.cutoff {
		return
	}
	if c.id != id {
		if c.owner == "You" {
			c.clear()
		} else {
			c.finish()
		}
		c.owner, c.id, c.text = "Agent", id, ""
		c.placeholder = false
	}
	c.text += cleanLine(text)
	c.draw()
}
func (c *liveConsole) replyEnd(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.owner == "Agent" && c.id == id {
		c.finish()
	}
}
