// Package audio provides real microphone capture and speaker playback via
// malgo (cgo bindings to miniaudio), plus the small amount of raw-audio
// plumbing a one-shot voice message needs: WAV encode/decode and linear
// resampling.
package audio

// SampleRate and Channels are the fixed capture/playback format used
// throughout this module: 48kHz mono. FrameSamples is the size of one 20ms
// chunk, the unit Mic delivers on Frames() and Recorder accumulates — an
// arbitrary but conventional chunk size, no longer tied to any RTP clock
// now that recorded audio goes out as a WAV file rather than a live Opus
// track.
const (
	SampleRate   = 48000
	Channels     = 1
	FrameMS      = 20
	FrameSamples = SampleRate * FrameMS / 1000 // 960
)
