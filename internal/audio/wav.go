package audio

import (
	"encoding/binary"
	"fmt"
)

// BytesToInt16 decodes little-endian PCM16 bytes into samples.
func BytesToInt16(b []byte) []int16 {
	n := len(b) / 2
	out := make([]int16, n)
	for i := 0; i < n; i++ {
		out[i] = int16(binary.LittleEndian.Uint16(b[i*2:]))
	}
	return out
}

// Int16ToBytes encodes samples as little-endian PCM16 bytes.
func Int16ToBytes(samples []int16) []byte {
	out := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(s))
	}
	return out
}

// DownmixToMono averages an arbitrary number of interleaved channels down to
// one. The agent's voice-message reply is always mono (see its
// internal/livekit/voice_mode.go), but a recorded WAV this client uploads is
// captured mono to begin with too — this only guards the decode side against
// that ever changing.
func DownmixToMono(samples []int16, channels int) []int16 {
	if channels <= 1 {
		return samples
	}
	n := len(samples) / channels
	out := make([]int16, n)
	for i := 0; i < n; i++ {
		var sum int32
		for c := 0; c < channels; c++ {
			sum += int32(samples[i*channels+c])
		}
		out[i] = int16(sum / int32(channels))
	}
	return out
}

// Resample linearly interpolates mono PCM16 samples from fromRate to toRate.
// Not a high-quality resampler, but dependency-free and good enough for
// speech — used to bring the agent's synthesized reply (whatever rate its
// TTS provider produced) to this module's fixed 48kHz playback rate.
func Resample(samples []int16, fromRate, toRate int) []int16 {
	if fromRate == toRate || len(samples) == 0 {
		return samples
	}
	outLen := int(int64(len(samples)) * int64(toRate) / int64(fromRate))
	if outLen <= 0 {
		return nil
	}
	out := make([]int16, outLen)
	ratio := float64(fromRate) / float64(toRate)
	for i := range out {
		srcPos := float64(i) * ratio
		idx := int(srcPos)
		frac := srcPos - float64(idx)
		if idx >= len(samples)-1 {
			out[i] = samples[len(samples)-1]
			continue
		}
		a, b := float64(samples[idx]), float64(samples[idx+1])
		out[i] = int16(a + (b-a)*frac)
	}
	return out
}

// DecodeWAV parses a PCM16 WAV file (any channel count / sample rate) and
// returns the interleaved samples. Only uncompressed PCM (format tag 1) is
// supported, which is all the agent ever produces. Mirrors (and is kept in
// sync by hand with) the agent's own internal/audio.DecodeWAV — unreachable
// here across module boundaries, so duplicated.
func DecodeWAV(data []byte) (samples []int16, sampleRate int, channels int, err error) {
	if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return nil, 0, 0, fmt.Errorf("audio: not a RIFF/WAVE file")
	}

	var (
		bitsPerSample int
		haveFmt       bool
		pcm           []byte
		haveData      bool
	)

	pos := 12
	for pos+8 <= len(data) {
		chunkID := string(data[pos : pos+4])
		chunkSize := int(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
		body := pos + 8
		if body+chunkSize > len(data) {
			chunkSize = len(data) - body // tolerate truncated trailing chunk
		}

		switch chunkID {
		case "fmt ":
			if chunkSize < 16 {
				return nil, 0, 0, fmt.Errorf("audio: fmt chunk too small")
			}
			formatTag := binary.LittleEndian.Uint16(data[body : body+2])
			if formatTag != 1 {
				return nil, 0, 0, fmt.Errorf("audio: unsupported WAV format tag %d (only PCM is supported)", formatTag)
			}
			channels = int(binary.LittleEndian.Uint16(data[body+2 : body+4]))
			sampleRate = int(binary.LittleEndian.Uint32(data[body+4 : body+8]))
			bitsPerSample = int(binary.LittleEndian.Uint16(data[body+14 : body+16]))
			haveFmt = true
		case "data":
			pcm = data[body : body+chunkSize]
			haveData = true
		}

		pos = body + chunkSize
		if chunkSize%2 == 1 {
			pos++ // chunks are word-aligned
		}
	}

	if !haveFmt || !haveData {
		return nil, 0, 0, fmt.Errorf("audio: missing fmt or data chunk")
	}
	if bitsPerSample != 16 {
		return nil, 0, 0, fmt.Errorf("audio: unsupported bits-per-sample %d (only 16-bit PCM is supported)", bitsPerSample)
	}

	return BytesToInt16(pcm), sampleRate, channels, nil
}

// EncodeWAV writes interleaved PCM16 samples as a WAV file.
func EncodeWAV(samples []int16, sampleRate, channels int) []byte {
	pcm := Int16ToBytes(samples)
	blockAlign := channels * 2
	byteRate := sampleRate * blockAlign
	dataSize := len(pcm)

	buf := make([]byte, 0, 44+dataSize)
	buf = append(buf, "RIFF"...)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(36+dataSize))
	buf = append(buf, "WAVE"...)

	buf = append(buf, "fmt "...)
	buf = binary.LittleEndian.AppendUint32(buf, 16)
	buf = binary.LittleEndian.AppendUint16(buf, 1) // PCM
	buf = binary.LittleEndian.AppendUint16(buf, uint16(channels))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(sampleRate))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(byteRate))
	buf = binary.LittleEndian.AppendUint16(buf, uint16(blockAlign))
	buf = binary.LittleEndian.AppendUint16(buf, 16) // bits per sample

	buf = append(buf, "data"...)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(dataSize))
	buf = append(buf, pcm...)

	return buf
}
