package audio

import (
	"encoding/binary"
	"fmt"
	"sync"

	"github.com/gen2brain/malgo"
)

// Speaker plays queued mono PCM16 samples through the default output
// device. Playback pacing comes entirely from the sound card's own callback
// — contrast internal/livekit/live_mode.go's trackAudioSink in the agent,
// which has to invent a time.Ticker-paced writer because nothing else
// drives an RTP track's send rate.
type Speaker struct {
	device *malgo.Device

	mu  sync.Mutex
	buf []int16
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

func (s *Speaker) Start() error { return s.device.Start() }

func (s *Speaker) Stop() {
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
	s.mu.Unlock()
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
	s.mu.Unlock()
}
