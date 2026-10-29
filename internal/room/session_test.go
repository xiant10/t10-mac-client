package room

import (
	"context"
	"testing"
)

func TestExchangeWaitsForAllStreams(t *testing.T) {
	ex := &exchange{done: make(chan error, 1), voice: true, terminal: true, expected: 2}
	assertWaiting := func() {
		t.Helper()
		ex.complete()
		select {
		case <-ex.done:
			t.Fatal("exchange completed before all data arrived")
		default:
		}
	}
	assertWaiting()
	ex.textDone = true
	assertWaiting()
	ex.next = 2
	assertWaiting()
	ex.transcriptDone = true
	ex.complete()
	select {
	case err := <-ex.done:
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("exchange did not complete")
	}
}

func TestExchangesKeepIdentityAndRemoveLateReplies(t *testing.T) {
	s := &Session{pending: make(map[string]*exchange)}
	textID, text := s.begin(context.Background(), false)
	voiceID, voice := s.begin(context.Background(), true)
	if textID == voiceID {
		t.Fatal("request IDs reused")
	}
	voice.terminal = true
	voice.textDone = true
	voice.transcriptDone = true
	voice.expected = 1
	voice.next = 1
	voice.complete()
	select {
	case <-text.done:
		t.Fatal("voice reply completed text exchange")
	default:
	}
	s.remove(voiceID)
	if s.pending[voiceID] != nil {
		t.Fatal("late reply can still target completed exchange")
	}
	if s.pending[textID] != text {
		t.Fatal("unrelated exchange removed")
	}
}
