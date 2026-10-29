package room

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestLiveConsoleInterruptionRejectsLateReplyAndClose(t *testing.T) {
	var out bytes.Buffer
	c := newLiveConsole(&out)
	c.reply(10, "old", "Already heard")
	c.state(20, "interrupted")
	c.state(21, "listening")
	c.interim(22, "user", "new thought")
	c.reply(10, "old", " but never spoken")
	c.replyEnd("old")
	if c.owner != "You" || c.text != "new thought" {
		t.Fatalf("late reply stole caller line: %+v", c)
	}
	if strings.Contains(out.String(), "never spoken") || !strings.Contains(out.String(), "Already heard [interrupted]") {
		t.Fatal(out.String())
	}
	c.transcript(30, "corrected thought")
	c.interim(22, "user", "late interim")
	if strings.Contains(out.String(), "late interim") {
		t.Fatal("interim resurrected after final")
	}
}
func TestLiveConsoleThinkingCanArriveBeforeTranscript(t *testing.T) {
	var out bytes.Buffer
	c := newLiveConsole(&out)
	c.state(10, "listening")
	c.interim(11, "u", "guess")
	c.state(30, "thinking")
	c.transcript(20, "confirmed")
	if c.owner != "Agent" || c.text != "[thinking]" {
		t.Fatalf("no working indicator after final: %+v", c)
	}
	c.state(5, "listening")
	if c.owner != "Agent" {
		t.Fatal("old state replaced current state")
	}
}

// Replays carriage returns, erase-line, full-width Chinese characters, and
// terminal wrapping. Raw escape-output assertions miss the duplicated rows.
func terminalRows(output string, columns int) []string {
	rows := [][]rune{make([]rune, columns)}
	row, col := 0, 0
	next := func() {
		row++
		col = 0
		if row == len(rows) {
			rows = append(rows, make([]rune, columns))
		}
	}
	for len(output) > 0 {
		if strings.HasPrefix(output, "\x1b[2K") {
			rows[row] = make([]rune, columns)
			output = output[4:]
			continue
		}
		r, n := utf8.DecodeRuneInString(output)
		output = output[n:]
		switch r {
		case '\r':
			col = 0
		case '\n':
			next()
		default:
			size := runeColumns(r)
			if size == 0 {
				continue
			}
			if col+size > columns {
				next()
			}
			rows[row][col] = r
			for j := 1; j < size; j++ {
				rows[row][col+j] = -1
			}
			col += size
		}
	}
	var result []string
	for _, cells := range rows {
		var line strings.Builder
		for _, r := range cells {
			if r == -1 {
				continue
			}
			if r == 0 {
				line.WriteRune(' ')
			} else {
				line.WriteRune(r)
			}
		}
		if text := strings.TrimRight(line.String(), " "); text != "" {
			result = append(result, text)
		}
	}
	return result
}

func TestLiveConsoleWrappedChineseReplyIsPrintedExactlyOnce(t *testing.T) {
	var out bytes.Buffer
	c := newLiveConsole(&out)
	c.columns = func() int { return 24 }
	reply := "杭州是一座风景优美的城市，以西湖和龙井茶闻名。欢迎你来杭州游玩。"
	for _, r := range reply {
		c.reply(30, "reply", string(r))
	}
	c.replyEnd("reply")
	got := strings.Join(terminalRows(out.String(), 24), "")
	if got != "Agent: "+reply {
		t.Fatalf("terminal duplicated or corrupted wrapped reply: %q", got)
	}
}

func TestLiveConsoleLateTranscriptDoesNotSplitOrDuplicateReply(t *testing.T) {
	var out bytes.Buffer
	c := newLiveConsole(&out)
	c.columns = func() int { return 40 }
	c.stateForUtterance(1, 1, "listening")
	c.updateInterim(2, 1, "u", "wrong guess", true)
	c.reply(5, "r", "First clause, ")
	c.state(4, "thinking") // Scheduled after reply data by the SDK.
	c.finalTranscript(3, 1, "corrected question")
	c.reply(5, "r", "second clause.")
	c.replyEnd("r")
	got := strings.Join(terminalRows(out.String(), 40), "\n")
	if got != "You: corrected question\nAgent: First clause, second clause." {
		t.Fatalf("terminal = %q", got)
	}
}

func TestLiveConsoleOlderFinalPreservesNewerUtterance(t *testing.T) {
	var out bytes.Buffer
	c := newLiveConsole(&out)
	c.updateInterim(2, 1, "old", "first half", true)
	c.stateForUtterance(3, 2, "listening")
	c.updateInterim(4, 2, "new", "second half", true)
	c.finalTranscript(5, 1, "First half.") // Committed after the next speech began.
	if c.owner != "You" || c.text != "second half" {
		t.Fatalf("older final erased current speech: %+v", c)
	}
	c.updateInterim(2, 1, "old", "late old guess", true)
	if c.text != "second half" {
		t.Fatal("stale interim replaced new speech")
	}
	c.finalTranscript(6, 2, "Second half.")
	got := strings.Join(terminalRows(out.String(), 80), "\n")
	if got != "You: First half.\nYou: Second half." {
		t.Fatalf("terminal = %q", got)
	}
}
