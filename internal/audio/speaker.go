package audio

import (
	"encoding/binary"
	"fmt"
	"sync"
	"time"

	"github.com/gen2brain/malgo"
)

// Speaker plays queued mono PCM16 samples through the default output
// device. Playback pacing comes entirely from the sound card's own callback
// — contrast internal/livekit/live_mode.go's trackAudioSink in the agent,
// which has to invent a time.Ticker-paced writer because nothing else
// drives an RTP track's send rate.
type Speaker struct {
	device *malgo.Device
	vpio   *vpioDuplex

	mu           sync.Mutex
	buf          []int16
	lastRendered time.Time
}

// NewSpeaker opens (but does not start) the default playback device on ctx.
func NewSpeaker(ctx *malgo.AllocatedContext) (*Speaker, error) {
	s := &Speaker{}

	cfg := malgo.DefaultDeviceConfig(malgo.Playback)
	cfg.Playback.Format = malgo.FormatS16
	cfg.Playback.Channels = Channels
	cfg.SampleRate = SampleRate

	device, err := malgo.InitDevice(ctx.Context, cfg, malgo.DeviceCallbacks{
		Data: s.onData,
	})
	if err != nil {
		return nil, fmt.Errorf("init playback device: %w", err)
	}
	s.device = device
	return s, nil
}

func (s *Speaker) Start() error {
	if s.vpio != nil {
		return s.vpio.start()
	}
	return s.device.Start()
}

func (s *Speaker) Stop() {
	if s.vpio != nil {
		s.vpio.stop()
		return
	}
	_ = s.device.Stop()
	s.device.Uninit()
}

// Push enqueues decoded PCM16 samples for playback.
func (s *Speaker) Push(samples []int16) {
	s.mu.Lock()
	s.buf = append(s.buf, samples...)
	s.mu.Unlock()
}

// Drain discards any queued-but-not-yet-played audio.
func (s *Speaker) Drain() {
	s.mu.Lock()
	s.buf = s.buf[:0]
	s.lastRendered = time.Time{}
	s.mu.Unlock()
}

// IsPlaying reports whether the speaker has queued samples or rendered audio
// within the recent acoustic tail window.
func (s *Speaker) IsPlaying() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.buf) > 0 {
		return true
	}
	return !s.lastRendered.IsZero() && time.Since(s.lastRendered) < 80*time.Millisecond
}

// onData runs on miniaudio's real-time audio thread. miniaudio pre-zeroes
// output before invoking the callback, so an underrun (not enough queued
// audio) is handled simply by writing nothing past the available samples —
// the rest plays as silence.
func (s *Speaker) onData(output, _ []byte, frameCount uint32) {
	n := int(frameCount)
	s.mu.Lock()
	avail := min(n, len(s.buf))
	for i := 0; i < avail; i++ {
		binary.LittleEndian.PutUint16(output[i*2:], uint16(s.buf[i]))
	}
	s.buf = s.buf[avail:]
	if avail > 0 {
		s.lastRendered = time.Now()
	}
	s.mu.Unlock()
}

func (s *Speaker) renderSamples(output []int16) int {
	s.mu.Lock()
	avail := min(len(output), len(s.buf))
	copy(output[:avail], s.buf[:avail])
	s.buf = s.buf[avail:]
	if avail > 0 {
		s.lastRendered = time.Now()
	}
	s.mu.Unlock()
	return avail
}
