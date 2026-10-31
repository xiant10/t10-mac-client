package audio

import (
	"encoding/binary"
	"fmt"

	"github.com/gen2brain/malgo"
)

// Mic captures the default input device and delivers fixed-size 960-sample
// (20ms) mono PCM16 frames on Frames(). The hardware's own capture callback
// supplies real-time pacing — no ticker needed, unlike a synthetic source.
type Mic struct {
	device *malgo.Device
	vpio   *vpioDuplex
	frames chan []int16

	pending []int16 // partial frame carried across callback invocations
}

// NewMic opens (but does not start) the default capture device on ctx.
func NewMic(ctx *malgo.AllocatedContext) (*Mic, error) {
	m := &Mic{
		frames: make(chan []int16, 100), // ~2s of audio; see onData for the overflow policy
	}

	cfg := malgo.DefaultDeviceConfig(malgo.Capture)
	cfg.Capture.Format = malgo.FormatS16
	cfg.Capture.Channels = Channels
	cfg.SampleRate = SampleRate

	device, err := malgo.InitDevice(ctx.Context, cfg, malgo.DeviceCallbacks{
		Data: m.onData,
	})
	if err != nil {
		return nil, fmt.Errorf("init capture device: %w", err)
	}
	m.device = device
	return m, nil
}

func (m *Mic) Start() error {
	if m.vpio != nil {
		return m.vpio.start()
	}
	return m.device.Start()
}

func (m *Mic) Stop() {
	if m.vpio != nil {
		m.vpio.stop()
		return
	}
	_ = m.device.Stop()
	m.device.Uninit()
}

// Frames yields fixed 960-sample (20ms) mono PCM16 frames as they're
// captured.
func (m *Mic) Frames() <-chan []int16 { return m.frames }

// onData runs on miniaudio's real-time audio thread, so it must never
// block: a full channel means the publish side has fallen behind, and the
// only safe response is to drop the newest frame rather than stall capture.
func (m *Mic) onData(_, input []byte, _ uint32) {
	n := len(input) / 2
	samples := make([]int16, n)
	for i := 0; i < n; i++ {
		samples[i] = int16(binary.LittleEndian.Uint16(input[i*2:]))
	}
	m.pushSamples(samples)
}

func (m *Mic) pushSamples(samples []int16) {
	m.pending = append(m.pending, samples...)

	for len(m.pending) >= FrameSamples {
		frame := make([]int16, FrameSamples)
		copy(frame, m.pending[:FrameSamples])
		m.pending = m.pending[FrameSamples:]
		select {
		case m.frames <- frame:
		default:
		}
	}
}
