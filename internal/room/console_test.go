package room

import (
	"bytes"
	"testing"
)

func TestConsole(t *testing.T) {
	tests := []struct {
		name string
		run  func(c *console)
		want string
	}{
		{
			name: "a typed exchange",
			run: func(c *console) {
				c.line("You: hello")
				c.agentChunk("Hi")
				c.agentChunk(" there!")
				c.agentEnd()
			},
			want: "You: hello\nAgent: Hi there!\n\n",
		},
		{
			name: "a voice message's heard-transcript line",
			run: func(c *console) {
				c.line("You (heard): what's the weather")
				c.agentChunk("Sunny.")
				c.agentEnd()
			},
			want: "You (heard): what's the weather\nAgent: Sunny.\n\n",
		},
		{
			name: "an empty reply closes nothing",
			run: func(c *console) {
				c.line("You: ...")
				c.agentEnd()
			},
			want: "You: ...\n",
		},
		{
			// Defensive case: a status line (e.g. a reply timeout) arrives
			// while a reply is still streaming. It must close the reply out
			// first rather than tearing the line in half.
			name: "a status line closes an unfinished reply first",
			run: func(c *console) {
				c.agentChunk("partial")
				c.line("(timed out waiting for a reply)")
			},
			want: "Agent: partial\n\n(timed out waiting for a reply)\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			tc.run(newConsole(&buf))
			if got := buf.String(); got != tc.want {
				t.Errorf("output = %q, want %q", got, tc.want)
			}
		})
	}
}
