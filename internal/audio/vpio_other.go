//go:build !darwin

package audio

import "errors"

type vpioDuplex struct{}

func NewVoiceProcessingDuplex() (*Speaker, *Mic, error) {
	return nil, nil, errors.New("VoiceProcessingIO is only supported on macOS")
}

func (d *vpioDuplex) start() error { return nil }
func (d *vpioDuplex) stop()        {}
