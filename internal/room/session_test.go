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

func TestBargeInOnsetBoostAndConsonantBridge(t *testing.T) {
	frame := make([]int16, 960)
	for i := range frame {
		frame[i] = 250
	}
	rms := frameRMS(frame)
	boosted := scaleFrameToRMS(frame, rms, 650, 3.5)
	if got := frameRMS(boosted); got < 600 {
		t.Fatalf("expected boosted RMS >= 600, got %v", got)
	}

	quiet := make([]int16, 960)
	bridged := bridgeConsonantDip(quiet, 0)
	if got := frameRMS(bridged); got < 520 {
		t.Fatalf("expected bridged consonant dip RMS >= 520, got %v", got)
	}

	sp := &smokeSpeaker{}
	ls := &LiveSession{speaker: sp}
	ls.triggerLocalBargeIn()
	if sp.drains != 1 {
		t.Fatalf("expected speaker drain on local barge-in, got %d", sp.drains)
	}
	if ls.isSpeakerActive() {
		t.Fatal("expected speaker to report inactive during local barge-in mute window")
	}
}
