package audio

import (
	"testing"
)

func TestSpeakerRenderAndMicPush(t *testing.T) {
	s := &Speaker{}
	s.Push([]int16{10, 20, 30})

	out := make([]int16, 5)
	n := s.renderSamples(out)
	if n != 3 {
		t.Fatalf("expected 3 rendered samples, got %d", n)
	}
	if out[0] != 10 || out[1] != 20 || out[2] != 30 {
		t.Fatalf("unexpected rendered samples: %v", out[:3])
	}

	m := &Mic{frames: make(chan []int16, 10)}
	chunk := make([]int16, FrameSamples+10)
	for i := range chunk {
		chunk[i] = int16(i)
	}
	m.pushSamples(chunk)

	select {
	case frame := <-m.Frames():
		if len(frame) != FrameSamples {
			t.Fatalf("expected frame length %d, got %d", FrameSamples, len(frame))
		}
		if frame[0] != 0 || frame[FrameSamples-1] != int16(FrameSamples-1) {
			t.Fatalf("unexpected frame contents")
		}
	default:
		t.Fatal("expected a frame on m.Frames()")
	}
}
