package audio

import "sync"

// Recorder turns Mic's continuous stream of frames into discrete "start
// recording ... stop recording" takes for one-shot voice messages. Mic's
// capture device runs for the whole life of the process (malgo's device
// type is start-once, not meant to be opened/closed per recording), so
// Recorder instead drains Frames() continuously in the background and only
// keeps what arrives between a Start/Stop pair, discarding the rest.
type Recorder struct {
	mu      sync.Mutex
	armed   bool
	samples []int16
}

// NewRecorder starts draining frames in the background immediately. Call
// Start/Stop to bracket one recording.
func NewRecorder(frames <-chan []int16) *Recorder {
	r := &Recorder{}
	go r.drain(frames)
	return r
}

func (r *Recorder) drain(frames <-chan []int16) {
	for f := range frames {
		r.mu.Lock()
		if r.armed {
			r.samples = append(r.samples, f...)
		}
		r.mu.Unlock()
	}
}

// Start arms the recorder, discarding anything left over from a previous
// take.
func (r *Recorder) Start() {
	r.mu.Lock()
	r.armed = true
	r.samples = nil
	r.mu.Unlock()
}

// Stop disarms the recorder and returns everything captured since Start.
func (r *Recorder) Stop() []int16 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.armed = false
	out := r.samples
	r.samples = nil
	return out
}
