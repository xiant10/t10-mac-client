//go:build darwin

package audio

/*
#cgo LDFLAGS: -framework AudioToolbox -framework CoreAudio -framework CoreFoundation

#include <AudioToolbox/AudioToolbox.h>
#include <CoreAudio/CoreAudio.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

extern int goVPIORender(uintptr_t userData, int16_t *output, uint32_t frameCount);
extern void goVPIOInput(uintptr_t userData, int16_t *input, uint32_t frameCount);

typedef struct {
	AudioUnit unit;
	uintptr_t userData;
	int16_t *inputBuf;
	uint32_t inputBufFrames;
} vpioHandle;

static OSStatus vpioRenderCallback(
	void *inRefCon,
	AudioUnitRenderActionFlags *ioActionFlags,
	const AudioTimeStamp *inTimeStamp,
	UInt32 inBusNumber,
	UInt32 inNumberFrames,
	AudioBufferList *ioData
) {
	vpioHandle *h = (vpioHandle *)inRefCon;
	if (!ioData || ioData->mNumberBuffers == 0) {
		return noErr;
	}
	void *dst = ioData->mBuffers[0].mData;
	UInt32 byteSize = ioData->mBuffers[0].mDataByteSize;
	memset(dst, 0, byteSize);

	int written = goVPIORender(h->userData, (int16_t *)dst, (uint32_t)inNumberFrames);
	if (written == 0 && ioActionFlags != NULL) {
		*ioActionFlags |= kAudioUnitRenderAction_OutputIsSilence;
	}
	return noErr;
}

static OSStatus vpioInputCallback(
	void *inRefCon,
	AudioUnitRenderActionFlags *ioActionFlags,
	const AudioTimeStamp *inTimeStamp,
	UInt32 inBusNumber,
	UInt32 inNumberFrames,
	AudioBufferList *ioData
) {
	vpioHandle *h = (vpioHandle *)inRefCon;
	if (inNumberFrames > h->inputBufFrames) {
		return noErr;
	}
	AudioBufferList bufList;
	bufList.mNumberBuffers = 1;
	bufList.mBuffers[0].mNumberChannels = 1;
	bufList.mBuffers[0].mDataByteSize = inNumberFrames * sizeof(int16_t);
	bufList.mBuffers[0].mData = h->inputBuf;

	OSStatus status = AudioUnitRender(h->unit, ioActionFlags, inTimeStamp, 1, inNumberFrames, &bufList);
	if (status != noErr) {
		return status;
	}
	goVPIOInput(h->userData, h->inputBuf, (uint32_t)inNumberFrames);
	return noErr;
}

static OSStatus vpioInit(uintptr_t userData, int sampleRate, int channels, vpioHandle **outHandle) {
	AudioComponentDescription desc;
	memset(&desc, 0, sizeof(desc));
	desc.componentType = kAudioUnitType_Output;
	desc.componentSubType = kAudioUnitSubType_VoiceProcessingIO;
	desc.componentManufacturer = kAudioUnitManufacturer_Apple;

	AudioComponent comp = AudioComponentFindNext(NULL, &desc);
	if (comp == NULL) {
		return -1;
	}

	AudioUnit unit = NULL;
	OSStatus status = AudioComponentInstanceNew(comp, &unit);
	if (status != noErr) {
		return status;
	}

	UInt32 enableIO = 1;
	// Enable input on Element 1 (microphone bus).
	status = AudioUnitSetProperty(unit, kAudioOutputUnitProperty_EnableIO,
		kAudioUnitScope_Input, 1, &enableIO, sizeof(enableIO));
	if (status != noErr) {
		AudioComponentInstanceDispose(unit);
		return status;
	}

	// Enable output on Element 0 (speaker bus).
	status = AudioUnitSetProperty(unit, kAudioOutputUnitProperty_EnableIO,
		kAudioUnitScope_Output, 0, &enableIO, sizeof(enableIO));
	if (status != noErr) {
		AudioComponentInstanceDispose(unit);
		return status;
	}

	AudioStreamBasicDescription streamFmt;
	memset(&streamFmt, 0, sizeof(streamFmt));
	streamFmt.mSampleRate = (Float64)sampleRate;
	streamFmt.mFormatID = kAudioFormatLinearPCM;
	streamFmt.mFormatFlags = kAudioFormatFlagIsSignedInteger | kAudioFormatFlagIsPacked;
	streamFmt.mFramesPerPacket = 1;
	streamFmt.mChannelsPerFrame = (UInt32)channels;
	streamFmt.mBitsPerChannel = 16;
	streamFmt.mBytesPerFrame = (UInt32)(channels * sizeof(int16_t));
	streamFmt.mBytesPerPacket = streamFmt.mBytesPerFrame;

	// Set stream format for speaker playback (Input scope of Element 0).
	status = AudioUnitSetProperty(unit, kAudioUnitProperty_StreamFormat,
		kAudioUnitScope_Input, 0, &streamFmt, sizeof(streamFmt));
	if (status != noErr) {
		AudioComponentInstanceDispose(unit);
		return status;
	}

	// Set stream format for microphone capture (Output scope of Element 1).
	status = AudioUnitSetProperty(unit, kAudioUnitProperty_StreamFormat,
		kAudioUnitScope_Output, 1, &streamFmt, sizeof(streamFmt));
	if (status != noErr) {
		AudioComponentInstanceDispose(unit);
		return status;
	}

	// Ensure voice processing (AEC + noise suppression) is not bypassed.
	UInt32 bypass = 0;
	AudioUnitSetProperty(unit, kAUVoiceIOProperty_BypassVoiceProcessing,
		kAudioUnitScope_Global, 0, &bypass, sizeof(bypass));
	AudioUnitSetProperty(unit, kAUVoiceIOProperty_BypassVoiceProcessing,
		kAudioUnitScope_Global, 1, &bypass, sizeof(bypass));

	// Enable Automatic Gain Control on VoiceProcessingIO.
	UInt32 agc = 1;
	AudioUnitSetProperty(unit, kAUVoiceIOProperty_VoiceProcessingEnableAGC,
		kAudioUnitScope_Global, 0, &agc, sizeof(agc));
	AudioUnitSetProperty(unit, kAUVoiceIOProperty_VoiceProcessingEnableAGC,
		kAudioUnitScope_Global, 1, &agc, sizeof(agc));

	vpioHandle *h = (vpioHandle *)calloc(1, sizeof(vpioHandle));
	h->unit = unit;
	h->userData = userData;
	h->inputBufFrames = 8192;
	h->inputBuf = (int16_t *)calloc(h->inputBufFrames, sizeof(int16_t));

	AURenderCallbackStruct renderCb;
	memset(&renderCb, 0, sizeof(renderCb));
	renderCb.inputProc = vpioRenderCallback;
	renderCb.inputProcRefCon = h;
	status = AudioUnitSetProperty(unit, kAudioUnitProperty_SetRenderCallback,
		kAudioUnitScope_Input, 0, &renderCb, sizeof(renderCb));
	if (status != noErr) {
		free(h->inputBuf);
		free(h);
		AudioComponentInstanceDispose(unit);
		return status;
	}

	AURenderCallbackStruct inputCb;
	memset(&inputCb, 0, sizeof(inputCb));
	inputCb.inputProc = vpioInputCallback;
	inputCb.inputProcRefCon = h;
	status = AudioUnitSetProperty(unit, kAudioOutputUnitProperty_SetInputCallback,
		kAudioUnitScope_Global, 1, &inputCb, sizeof(inputCb));
	if (status != noErr) {
		free(h->inputBuf);
		free(h);
		AudioComponentInstanceDispose(unit);
		return status;
	}

	status = AudioUnitInitialize(unit);
	if (status != noErr) {
		free(h->inputBuf);
		free(h);
		AudioComponentInstanceDispose(unit);
		return status;
	}

	*outHandle = h;
	return noErr;
}

static OSStatus vpioStart(vpioHandle *h) {
	if (!h || !h->unit) {
		return -1;
	}
	return AudioOutputUnitStart(h->unit);
}

static void vpioStop(vpioHandle *h) {
	if (!h) {
		return;
	}
	if (h->unit) {
		AudioOutputUnitStop(h->unit);
		AudioUnitUninitialize(h->unit);
		AudioComponentInstanceDispose(h->unit);
		h->unit = NULL;
	}
	if (h->inputBuf) {
		free(h->inputBuf);
		h->inputBuf = NULL;
	}
	free(h);
}
*/
import "C"

import (
	"fmt"
	"runtime/cgo"
	"sync"
	"unsafe"
)

// vpioDuplex manages a shared macOS VoiceProcessingIO AudioUnit that performs
// hardware/OS-level Acoustic Echo Cancellation (AEC) between Speaker output
// (Element 0) and Mic capture (Element 1).
type vpioDuplex struct {
	speaker *Speaker
	mic     *Mic

	mu      sync.Mutex
	handle  *C.vpioHandle
	cgoHdl  cgo.Handle
	started bool
	stopped bool
}

// NewVoiceProcessingDuplex opens a coupled Speaker + Mic backed by macOS's
// kAudioUnitSubType_VoiceProcessingIO AudioUnit, providing system-level
// Acoustic Echo Cancellation (AEC), noise suppression, and AGC.
func NewVoiceProcessingDuplex() (*Speaker, *Mic, error) {
	s := &Speaker{}
	m := &Mic{
		frames: make(chan []int16, 100),
	}
	d := &vpioDuplex{
		speaker: s,
		mic:     m,
	}
	d.cgoHdl = cgo.NewHandle(d)

	var h *C.vpioHandle
	status := C.vpioInit(C.uintptr_t(d.cgoHdl), C.int(SampleRate), C.int(Channels), &h)
	if status != 0 {
		d.cgoHdl.Delete()
		return nil, nil, fmt.Errorf("init VoiceProcessingIO audio unit: OSStatus %d", int(status))
	}
	d.handle = h
	s.vpio = d
	m.vpio = d
	return s, m, nil
}

func (d *vpioDuplex) start() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.started {
		return nil
	}
	status := C.vpioStart(d.handle)
	if status != 0 {
		return fmt.Errorf("start VoiceProcessingIO audio unit: OSStatus %d", int(status))
	}
	d.started = true
	return nil
}

func (d *vpioDuplex) stop() {
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		return
	}
	d.stopped = true
	h := d.handle
	d.handle = nil
	cgoHdl := d.cgoHdl
	d.mu.Unlock()

	if h != nil {
		C.vpioStop(h)
	}
	if cgoHdl != 0 {
		cgoHdl.Delete()
	}
}

//export goVPIORender
func goVPIORender(userData C.uintptr_t, output *C.int16_t, frameCount C.uint32_t) C.int {
	h := cgo.Handle(userData)
	d, ok := h.Value().(*vpioDuplex)
	if !ok || d == nil || d.speaker == nil {
		return 0
	}
	n := int(frameCount)
	outSlice := unsafe.Slice((*int16)(unsafe.Pointer(output)), n)
	return C.int(d.speaker.renderSamples(outSlice))
}

//export goVPIOInput
func goVPIOInput(userData C.uintptr_t, input *C.int16_t, frameCount C.uint32_t) {
	h := cgo.Handle(userData)
	d, ok := h.Value().(*vpioDuplex)
	if !ok || d == nil || d.mic == nil {
		return
	}
	n := int(frameCount)
	inSlice := unsafe.Slice((*int16)(unsafe.Pointer(input)), n)
	d.mic.pushSamples(inSlice)
}
