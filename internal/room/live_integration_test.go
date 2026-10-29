package room

import (
	"bytes"
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joho/godotenv"
	"t10-client/internal/audio"
)

type smokeSpeaker struct {
	mu      sync.Mutex
	nonzero int
	drains  int
}

func (s *smokeSpeaker) Push(pcm []int16) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range pcm {
		if v > 100 || v < -100 {
			s.nonzero++
		}
	}
}
func (s *smokeSpeaker) Drain() { s.mu.Lock(); defer s.mu.Unlock(); s.drains++ }

// Opt-in: a real agent must be running in ux-smoke. No microphone or audio
// device is opened; the ordinary live client publishes WAV PCM and receives
// real provider audio through its normal track and caption handlers.
func TestLiveSessionProviderSmoke(t *testing.T) {
	path := os.Getenv("T10_LIVE_SMOKE_WAV")
	if path == "" {
		t.Skip("set T10_LIVE_SMOKE_WAV and run an agent in ux-smoke")
	}
	env, err := godotenv.Read("../../.env")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pcm, rate, channels, err := audio.DecodeWAV(data)
	if err != nil {
		t.Fatal(err)
	}
	pcm = audio.Resample(audio.DownmixToMono(pcm, channels), rate, audio.SampleRate)
	pcm = append(make([]int16, audio.SampleRate), pcm...)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	frames := make(chan []int16)
	speaker := &smokeSpeaker{}
	s, err := ConnectLive(ctx, Config{URL: env["LIVEKIT_URL"], APIKey: env["LIVEKIT_API_KEY"], APISecret: env["LIVEKIT_API_SECRET"]}, "ux-smoke", "client-smoke", frames, speaker)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Disconnect()
	var output bytes.Buffer
	s.con.mu.Lock()
	s.con.w = &output
	s.con.columns = func() int { return 80 }
	s.con.mu.Unlock()
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for off := 0; ; off += audio.FrameSamples {
			frame := make([]int16, audio.FrameSamples)
			if off < len(pcm) {
				copy(frame, pcm[off:min(off+audio.FrameSamples, len(pcm))])
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			select {
			case <-ctx.Done():
				return
			case frames <- frame:
			}
		}
	}()
	var sawInterimBeforeFinal bool
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			t.Fatal("live client did not receive transcript, audio, and idle within 40s")
		case <-ticker.C:
			speaker.mu.Lock()
			heard := speaker.nonzero > 1000
			speaker.mu.Unlock()
			s.con.mu.Lock()
			text := output.String()
			idle := s.con.lastState == "idle"
			if s.con.owner == "You" && !s.con.placeholder && s.con.text != "" && s.con.finalUtteranceID == 0 {
				sawInterimBeforeFinal = true
			}
			s.con.mu.Unlock()
			if heard && idle && strings.Contains(text, "You: ") {
				if !sawInterimBeforeFinal {
					t.Fatal("final reply succeeded but no streaming interim transcript was visible")
				}
				rendered := strings.Join(terminalRows(text, 80), "")
				if strings.Count(rendered, "Agent: ") != 1 {
					t.Fatalf("duplicate/missing agent reply on terminal: %q", rendered)
				}
				t.Log("verified streaming interim before final, one rendered agent reply, decoded speech audio, and idle")
				t.Logf("rendered terminal:\n%s", strings.Join(terminalRows(text, 80), "\n"))
				return
			}
		}
	}
}
