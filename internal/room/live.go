package room

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"strconv"
	"sync"
	"time"

	lkproto "github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"

	"t10-client/internal/audio/opuscodec"
	"t10-client/internal/roomprotocol"
	"t10-client/internal/telemetry"
)

// LiveOption configures optional parameters for ConnectLive.
type LiveOption func(*liveOptions)

type liveOptions struct {
	consoleOut io.Writer
}

// WithConsoleOutput sets the output writer for live captions and status.
// Defaults to os.Stdout if not provided.
func WithConsoleOutput(w io.Writer) LiveOption {
	return func(o *liveOptions) {
		if w != nil {
			o.consoleOut = w
		}
	}
}

// LiveSession owns a continuous microphone track and the agent's playback.
// Text/voice-message sessions remain independent and do not consume its mic.
type liveSpeaker interface {
	Push([]int16)
	Drain()
}

type LiveSession struct {
	room           *lksdk.Room
	cancel         context.CancelFunc
	done           chan struct{}
	con            *liveConsole
	speaker        liveSpeaker
	mu             sync.Mutex
	muted          bool
	localMuteUntil time.Time
	stateTime      int64
	endFirstAudio  func(error) // the client.first_audio span still waiting on audio
}

func ConnectLive(ctx context.Context, cfg Config, roomName, identity string, micFrames <-chan []int16, speaker liveSpeaker, opts ...LiveOption) (_ *LiveSession, err error) {
	options := liveOptions{consoleOut: os.Stdout}
	for _, opt := range opts {
		opt(&options)
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &LiveSession{cancel: cancel, done: make(chan struct{}), speaker: speaker, con: newLiveConsole(options.consoleOut)}
	connectCtx, endConnect := telemetry.StartTrace(ctx, "client.connect")
	defer func() { endConnect(err) }()
	_, tokenEnd := telemetry.StartStage(connectCtx, "room.mint_token")
	serverURL, token, _, canPublish, err := cfg.ResolveCredentials(ctx, roomName, identity)
	tokenEnd(err)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("acquire room credentials: %w", err)
	}
	encoder, err := opuscodec.NewEncoder()
	if err != nil {
		cancel()
		return nil, err
	}
	cb := lksdk.NewRoomCallback()
	cb.OnTrackSubscribed = func(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
		if track.Kind() != webrtc.RTPCodecTypeAudio || pub.Name() != "agent-voice" {
			return
		}
		slog.InfoContext(ctx, "receiving agent audio", "identity", rp.Identity())
		go s.play(ctx, track)
	}
	cb.OnAttributesChanged = func(changed map[string]string, rp lksdk.Participant) {
		if rp.Identity() == identity {
			return
		}
		if state, ok := changed[roomprotocol.AttrLKAgentState]; ok {
			s.handleAgentState(state)
		}
	}
	_, connectEnd := telemetry.StartStage(connectCtx, "room.connect_live")
	r, err := lksdk.ConnectToRoomWithToken(serverURL, token, cb, lksdk.WithAutoSubscribe(true))
	connectEnd(err)
	if err != nil {
		cancel()
		return nil, err
	}
	s.room = r
	sessionCtx := telemetry.WithSessionID(ctx, r.LocalParticipant.SID())
	fail := func(err error) (*LiveSession, error) { cancel(); r.Disconnect(); return nil, err }
	if err := s.registerLiveHandlers(sessionCtx, r); err != nil {
		return fail(err)
	}
	if canPublish {
		track, err := lksdk.NewLocalSampleTrack(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: opuscodec.SampleRate, Channels: opuscodec.RTPChannels})
		if err != nil {
			return fail(err)
		}
		_, pubEnd := telemetry.StartStage(connectCtx, "room.publish_track")
		_, pubErr := r.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{Name: "mic", Source: lkproto.TrackSource_MICROPHONE})
		pubEnd(pubErr)
		if pubErr != nil {
			return fail(pubErr)
		}
		go s.publishMic(sessionCtx, track, micFrames, encoder)
	} else {
		slog.WarnContext(sessionCtx, "room admission granted in listen-only mode (publishing not permitted)")
	}
	return s, nil
}

func (s *LiveSession) Disconnect() {
	s.cancel()
	s.room.Disconnect()
	<-s.done
	s.speaker.Drain()
}

func (s *LiveSession) handleAgentState(state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch state {
	case "interrupted", "listening":
		s.muted = true
		s.localMuteUntil = time.Time{}
		s.speaker.Drain()
	case "speaking":
		s.muted = false
		s.localMuteUntil = time.Time{}
	}
}

func (s *LiveSession) play(ctx context.Context, track *webrtc.TrackRemote) {
	decoder, err := opuscodec.NewDecoder()
	if err != nil {
		slog.ErrorContext(ctx, "decode agent audio", "err", err)
		return
	}
	builder := samplebuilder.New(50, &codecs.OpusPacket{}, opuscodec.SampleRate)
	for {
		if ctx.Err() != nil {
			return
		}
		if sample := builder.Pop(); sample != nil {
			pcm, err := decoder.DecodeFrame(sample.Data)
			if err != nil {
				continue
			}
			s.playFrame(pcm)
			continue
		}
		pkt, _, err := track.ReadRTP()
		if err != nil || ctx.Err() != nil {
			return
		}
		builder.Push(pkt)
	}
}

// playFrame hands one decoded frame to the speaker. A state transition and a
// push use the same lock: no late packet can refill the speaker after
// interruption until the next speaking state.
func (s *LiveSession) playFrame(pcm []int16) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.muted || (!s.localMuteUntil.IsZero() && !time.Now().After(s.localMuteUntil)) {
		return
	}
	s.speaker.Push(pcm)
	s.finishFirstAudio()
}

// awaitFirstAudio times, in the agent's trace for this turn, how long the
// caller waits between the agent starting to speak and it reaching the speaker.
// Callers hold mu.
func (s *LiveSession) awaitFirstAudio(ctx context.Context) {
	s.finishFirstAudio()
	_, s.endFirstAudio = telemetry.StartStage(ctx, "client.first_audio")
}

// finishFirstAudio closes the span, if one is open. Callers hold mu.
func (s *LiveSession) finishFirstAudio() {
	if s.endFirstAudio != nil {
		s.endFirstAudio(nil)
		s.endFirstAudio = nil
	}
}

// Explicit ordering survives same-millisecond transitions and SDK handler
// goroutines being scheduled in a different order from their wire messages.
func eventPosition(reader *lksdk.TextStreamReader) int64 {
	if n, err := strconv.ParseInt(reader.Info.Attributes[roomprotocol.AttrEventOrder], 10, 64); err == nil && n > 0 {
		return n
	}
	return reader.Info.Timestamp
}
func utteranceNumber(reader *lksdk.TextStreamReader) uint64 {
	n, _ := strconv.ParseUint(reader.Info.Attributes[roomprotocol.AttrUtteranceID], 10, 64)
	return n
}

// registerLiveHandlers registers all text-stream handlers (state, transcript,
// interim, chat) on the room for the LiveSession.
func (s *LiveSession) registerLiveHandlers(ctx context.Context, r *lksdk.Room) error {
	handlers := map[string]func(*lksdk.TextStreamReader, string){
		roomprotocol.TopicState: func(reader *lksdk.TextStreamReader, _ string) {
			state := reader.ReadAll()
			turnCtx := telemetry.ContextWithTraceparent(ctx, reader.Info.Attributes[roomprotocol.AttrTraceparent])
			slog.InfoContext(turnCtx, "live call: state received", "state", state, "event_order", eventPosition(reader), "utterance_id", utteranceNumber(reader))
			s.mu.Lock()
			if eventPosition(reader) > s.stateTime {
				s.stateTime = eventPosition(reader)
				switch state {
				case "interrupted", "listening":
					s.muted = true
					s.localMuteUntil = time.Time{}
					s.speaker.Drain()
					s.finishFirstAudio()
				case "speaking":
					s.muted = false
					s.localMuteUntil = time.Time{}
					s.awaitFirstAudio(turnCtx)
				}
			}
			s.mu.Unlock()
			s.con.stateForUtterance(eventPosition(reader), utteranceNumber(reader), state)
		},
		roomprotocol.TopicTranscript: func(reader *lksdk.TextStreamReader, _ string) {
			s.con.finalTranscript(eventPosition(reader), utteranceNumber(reader), reader.ReadAll())
		},
		roomprotocol.TopicLKTranscription: func(reader *lksdk.TextStreamReader, _ string) {
			isFinal := reader.Info.Attributes[roomprotocol.AttrLKTranscriptionFinal] == "true"
			if isFinal {
				s.con.finalTranscript(eventPosition(reader), utteranceNumber(reader), reader.ReadAll())
			}
		},
		roomprotocol.TopicTranscriptInterim: func(reader *lksdk.TextStreamReader, _ string) {
			if reader.Info.Attributes[roomprotocol.AttrFormat] == roomprotocol.FormatSnapshotJSONL {
				scanner := bufio.NewScanner(reader)
				scanner.Buffer(make([]byte, 4096), 1024*1024)
				for scanner.Scan() {
					var update struct {
						Text string `json:"text"`
					}
					if json.Unmarshal(scanner.Bytes(), &update) == nil {
						slog.DebugContext(ctx, "live call: interim received", "utterance_id", utteranceNumber(reader), "chars", len([]rune(update.Text)))
						s.con.updateInterim(eventPosition(reader), utteranceNumber(reader), reader.Info.Id, update.Text, true)
					}
				}
				if err := scanner.Err(); err != nil {
					slog.ErrorContext(ctx, "live call: interim stream failed", "err", err)
				}
				return
			}
			for {
				ch, _, err := reader.ReadRune()
				if err != nil {
					return
				}
				s.con.interim(eventPosition(reader), reader.Info.Id, string(ch))
			}
		},
		roomprotocol.TopicChat: func(reader *lksdk.TextStreamReader, _ string) {
			for {
				ch, _, err := reader.ReadRune()
				if err != nil {
					break
				}
				s.con.reply(eventPosition(reader), reader.Info.Id, string(ch))
			}
			s.con.replyEnd(reader.Info.Id)
		},
	}
	for topic, handler := range handlers {
		if err := r.RegisterTextStreamHandler(topic, handler); err != nil {
			return err
		}
	}
	return nil
}

// isSpeakerActive reports whether the agent is currently speaking and the
// speaker is not already muted.
func (s *LiveSession) isSpeakerActive() bool {
	s.mu.Lock()
	muted := s.muted || (!s.localMuteUntil.IsZero() && time.Now().Before(s.localMuteUntil))
	s.mu.Unlock()
	if muted {
		return false
	}
	if p, ok := s.speaker.(interface{ IsPlaying() bool }); ok {
		return p.IsPlaying()
	}
	return true
}

func (s *LiveSession) triggerLocalBargeIn() {
	s.mu.Lock()
	s.localMuteUntil = time.Now().Add(650 * time.Millisecond)
	s.speaker.Drain()
	s.mu.Unlock()
}

func frameRMS(frame []int16) float64 {
	if len(frame) == 0 {
		return 0
	}
	var energy float64
	for _, v := range frame {
		f := float64(v)
		energy += f * f
	}
	return math.Sqrt(energy / float64(len(frame)))
}

func scaleFrameToRMS(frame []int16, currentRMS, targetRMS, maxGain float64) []int16 {
	if currentRMS <= 0 || currentRMS >= targetRMS {
		return frame
	}
	gain := min(targetRMS/currentRMS, maxGain)
	out := make([]int16, len(frame))
	for i, v := range frame {
		scaled := float64(v) * gain
		if scaled > math.MaxInt16 {
			scaled = math.MaxInt16
		} else if scaled < math.MinInt16 {
			scaled = math.MinInt16
		}
		out[i] = int16(scaled)
	}
	return out
}

// bridgeConsonantDip keeps up to 3 frames (60ms) of inter-syllable unvoiced
// consonant gap above the agent's 500 RMS threshold during an active barge-in
// onset so the agent's energy VAD does not reset v.speechMS = 0 mid-word.
func bridgeConsonantDip(frame []int16, currentRMS float64) []int16 {
	out := scaleFrameToRMS(frame, currentRMS, 560, 4.0)
	if frameRMS(out) >= 540 {
		return out
	}
	if &out[0] == &frame[0] {
		out = append([]int16(nil), frame...)
	}
	// Blend a gentle 75Hz low-frequency carrier (above Opus's 50Hz HPF, below
	// human speech formants) so the resampled 16kHz frame stays >= 520 RMS.
	const amp = 780.0
	const twoPiF = 2 * math.Pi * 75.0 / float64(opuscodec.SampleRate)
	for i := range out {
		v := float64(out[i]) + amp*math.Sin(twoPiF*float64(i))
		if v > math.MaxInt16 {
			v = math.MaxInt16
		} else if v < math.MinInt16 {
			v = math.MinInt16
		}
		out[i] = int16(v)
	}
	return out
}

// publishMic reads PCM frames from micFrames, encodes them as Opus, and
// publishes them to the given track until ctx is cancelled or micFrames closes.
func (s *LiveSession) publishMic(ctx context.Context, track *lksdk.LocalSampleTrack, micFrames <-chan []int16, encoder *opuscodec.Encoder) {
	defer close(s.done)
	var (
		frames          int
		peak            float64
		voicedFrames    int
		dipFrames       int
		assistRemaining int
	)
	for {
		select {
		case <-ctx.Done():
			return
		case frame, ok := <-micFrames:
			if !ok {
				return
			}
			rms := frameRMS(frame)
			speakerActive := s.isSpeakerActive()

			if speakerActive || assistRemaining > 0 {
				if rms >= 190 {
					voicedFrames++
					dipFrames = 0
					if speakerActive && (voicedFrames >= 2 || rms >= 340) {
						s.triggerLocalBargeIn()
						assistRemaining = 25 // ~500ms barge-in assist window
					} else if assistRemaining > 0 {
						assistRemaining--
					}
					// Restore gain on double-talk-attenuated onset frames so the
					// very first syllable crosses the agent's 500 RMS threshold.
					if rms < 650 {
						frame = scaleFrameToRMS(frame, rms, 650, 3.5)
						rms = frameRMS(frame)
					}
				} else if assistRemaining > 0 && voicedFrames >= 2 {
					dipFrames++
					assistRemaining--
					if dipFrames <= 3 {
						frame = bridgeConsonantDip(frame, rms)
						rms = frameRMS(frame)
					} else {
						voicedFrames = 0
						dipFrames = 0
						assistRemaining = 0
					}
				} else {
					voicedFrames = 0
					dipFrames = 0
					assistRemaining = 0
				}
			} else {
				voicedFrames = 0
				dipFrames = 0
			}

			encoded, err := encoder.EncodeFrame(frame)
			if err != nil {
				slog.ErrorContext(ctx, "encode microphone", "err", err)
				continue
			}
			if err := track.WriteSample(media.Sample{Data: encoded, Duration: opuscodec.FrameMS * time.Millisecond}, nil); err != nil {
				slog.ErrorContext(ctx, "publish microphone", "err", err)
				return
			}
			if frames < 100 {
				peak = math.Max(peak, rms)
				frames++
				if frames == 100 {
					slog.InfoContext(ctx, "microphone audio flowing", "frames", frames, "peak_frame_rms", int(peak))
				}
			}
		}
	}
}
