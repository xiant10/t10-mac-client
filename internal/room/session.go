// Package room wires a LiveKit room connection to the local mic/speaker: it
// mints a join token, connects, and exchanges pure-text chat and one-shot
// recorded voice messages with the agent — see Session.SendText and
// Session.SendVoice. There is no published or subscribed audio track here:
// unlike the agent's live-call mode, a voice message is a single WAV upload
// answered by ordered WAV reply chunks, not a continuous stream, so none of the
// RTP/Opus machinery a live call needs applies.
package room

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/livekit/protocol/auth"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"go.opentelemetry.io/otel/attribute"

	"t10-client/internal/audio"
	"t10-client/internal/roomprotocol"
	"t10-client/internal/telemetry"
)

// Config holds the LiveKit and Auth connection details needed to join a room.
type Config struct {
	URL       string
	APIKey    string
	APISecret string

	// Auth service configuration for admission
	AuthURL   string
	AuthToken string
	Tenant    string
	Username  string
	Password  string

	// Direct room token (if already acquired)
	RoomToken string
}

// replyTimeout bounds how long one exchange waits for the agent's reply
// before giving up and handing control back to the caller.
const replyTimeout = 60 * time.Second

// mintToken builds a JWT granting identity publish/subscribe access to
// roomName. Mirrors the agent's own internal/livekit.MintToken — unreachable
// here across module boundaries, so duplicated (it's ten lines).
func mintToken(cfg Config, identity, roomName string, validFor time.Duration) (string, error) {
	canPublish := true
	canSubscribe := true

	grant := &auth.VideoGrant{
		RoomJoin:     true,
		Room:         roomName,
		CanPublish:   &canPublish,
		CanSubscribe: &canSubscribe,
	}

	at := auth.NewAccessToken(cfg.APIKey, cfg.APISecret).
		SetIdentity(identity).
		SetVideoGrant(grant).
		SetValidFor(validFor)

	return at.ToJWT()
}

// exchange tracks one in-flight SendText/SendVoice call: the correlation
// context it was started under (so a reply handler's own logging ties back
// to the request that caused it) and a done signal the handler fires when
// the reply is fully in.
type exchange struct {
	ctx                                context.Context
	done                               chan error
	voice                              bool
	textDone, transcriptDone, terminal bool
	expected, next                     int
	audio                              map[int][]int16
}

func (ex *exchange) signal(err error) {
	select {
	case ex.done <- err:
	default:
	}
}
func (ex *exchange) complete() {
	if ex.terminal && ex.textDone && (!ex.voice || (ex.transcriptDone && ex.next == ex.expected)) {
		ex.signal(nil)
	}
}

// Session is an active LiveKit room connection that exchanges text and
// voice messages with the agent.
type Session struct {
	ctx     context.Context // carries the session ID
	room    *lksdk.Room
	speaker *audio.Speaker
	con     *console

	sendMu  sync.Mutex // serialize presentation and playback across exchanges
	mu      sync.Mutex
	pending map[string]*exchange
}

// Connect joins roomName as identity and registers the handlers for the
// agent's transcript, chat-reply, and voice-reply topics. speaker is where a
// voice message's synthesized reply gets played. Connecting is a trace of its
// own, and each subsequent SendText/SendVoice call is another: one per
// client-initiated request rather than one for the whole run.
func Connect(ctx context.Context, cfg Config, roomName, identity string, speaker *audio.Speaker) (_ *Session, err error) {
	baseCtx := ctx
	ctx, endConnect := telemetry.StartTrace(ctx, "client.connect")
	defer func() { endConnect(err) }()

	_, end := telemetry.StartStage(ctx, "room.mint_token")
	serverURL, token, _, _, err := cfg.ResolveCredentials(ctx, roomName, identity)
	end(err)
	if err != nil {
		return nil, fmt.Errorf("acquire room credentials: %w", err)
	}

	_, connectEnd := telemetry.StartStage(ctx, "room.connect")
	r, err := lksdk.ConnectToRoomWithToken(serverURL, token, lksdk.NewRoomCallback())
	connectEnd(err)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}

	s := &Session{ctx: telemetry.WithSessionID(baseCtx, r.LocalParticipant.SID()), room: r, speaker: speaker, con: newConsole(os.Stdout), pending: make(map[string]*exchange)}

	// The SDK invokes each registered handler in its own goroutine per
	// incoming stream (see server-sdk-go's Room.handleIncomingText: "go
	// streamHandlerCallback(...)"), so these can in principle fire
	// concurrently with each other even though the agent sends them in a
	// fixed order (transcript, then chat reply, then voice reply) for one
	// voice-message exchange. console is what makes that safe to print.
	if err := r.RegisterTextStreamHandler(roomprotocol.TopicTranscript, s.handleTranscript); err != nil {
		r.Disconnect()
		return nil, fmt.Errorf("register transcript handler: %w", err)
	}
	if err := r.RegisterTextStreamHandler(roomprotocol.TopicChat, s.handleChatReply); err != nil {
		r.Disconnect()
		return nil, fmt.Errorf("register chat handler: %w", err)
	}
	if err := r.RegisterByteStreamHandler(roomprotocol.TopicVoice, s.handleVoiceReply); err != nil {
		r.Disconnect()
		return nil, fmt.Errorf("register voice handler: %w", err)
	}

	if err := r.RegisterTextStreamHandler(roomprotocol.TopicMessageStatus, s.handleStatus); err != nil {
		r.Disconnect()
		return nil, err
	}
	return s, nil
}

func (s *Session) Disconnect() { s.room.Disconnect() }

// SendText sends one line of typed chat and blocks until the agent's full
// reply has been printed, or replyTimeout elapses.
func (s *Session) SendText(text string) {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	ctx, endTrace := telemetry.StartTrace(s.ctx, "client.exchange", attribute.String("turn.mode", "text"))

	s.con.line("You: " + text)

	id, ex := s.begin(ctx, false)
	defer s.remove(id)
	stageCtx, endStage := telemetry.StartStage(ctx, "room.chat_exchange", attribute.String("request_id", id))
	s.room.LocalParticipant.SendText(text, lksdk.StreamTextOptions{Topic: roomprotocol.TopicChat, Attributes: requestAttributes(stageCtx, id)})
	err := s.wait(ex)
	endStage(err)
	endTrace(err)
}

// requestAttributes names a message and starts the agent off in its trace.
func requestAttributes(ctx context.Context, id string) map[string]string {
	return map[string]string{roomprotocol.AttrRequestID: id, roomprotocol.AttrTraceparent: telemetry.Traceparent(ctx)}
}

// SendVoice uploads a recorded WAV as a one-shot voice message and blocks
// until the agent's transcript, text reply, and synthesized audio reply have
// all arrived (the audio is queued to the speaker before this returns,
// though playback itself continues in the background), or replyTimeout
// elapses.
func (s *Session) SendVoice(wavBytes []byte) {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	ctx, endTrace := telemetry.StartTrace(s.ctx, "client.exchange", attribute.String("turn.mode", "voice"))

	id, ex := s.begin(ctx, true)
	defer s.remove(id)
	stageCtx, endStage := telemetry.StartStage(ctx, "room.voice_exchange", attribute.Int("audio.bytes", len(wavBytes)), attribute.String("request_id", id))
	w := s.room.LocalParticipant.StreamBytes(lksdk.StreamBytesOptions{
		Topic: roomprotocol.TopicVoice, MimeType: "audio/wav", Attributes: requestAttributes(stageCtx, id),
	})
	uploaded := make(chan struct{})
	callback := func() { close(uploaded) }
	w.Write(wavBytes, &callback)
	select {
	case <-uploaded:
		w.Close()
		err := s.wait(ex)
		endStage(err)
		endTrace(err)
	case <-time.After(replyTimeout):
		w.Close()
		s.con.line("(upload timed out)")
		err := fmt.Errorf("upload timed out")
		endStage(err)
		endTrace(err)
	}
}

func (s *Session) wait(ex *exchange) error {
	select {
	case err := <-ex.done:
		return err
	case <-time.After(replyTimeout):
		s.con.line("(timed out waiting for a reply)")
		return fmt.Errorf("timed out waiting for a reply")
	}
}

func (s *Session) begin(ctx context.Context, voice bool) (string, *exchange) {
	id := rand.Text()
	ex := &exchange{ctx: ctx, done: make(chan error, 1), voice: voice, audio: make(map[int][]int16)}
	s.mu.Lock()
	s.pending[id] = ex
	s.mu.Unlock()
	return id, ex
}
func (s *Session) remove(id string) { s.mu.Lock(); delete(s.pending, id); s.mu.Unlock() }

func (s *Session) handleTranscript(reader *lksdk.TextStreamReader, _ string) {
	text := reader.ReadAll()
	s.mu.Lock()
	defer s.mu.Unlock()
	if ex := s.pending[reader.Info.Attributes[roomprotocol.AttrRequestID]]; ex != nil {
		s.con.line("You (heard): " + text)
		ex.transcriptDone = true
		ex.complete()
	}
}

func (s *Session) handleChatReply(reader *lksdk.TextStreamReader, _ string) {
	id := reader.Info.Attributes[roomprotocol.AttrRequestID]
	for {
		ch, _, err := reader.ReadRune()
		s.mu.Lock()
		ex := s.pending[id]
		if ex != nil {
			if err == nil {
				s.con.agentChunk(string(ch))
			} else {
				s.con.agentEnd()
				if err != io.EOF {
					s.con.line("(reply interrupted)")
					ex.signal(err)
				} else {
					ex.textDone = true
					ex.complete()
				}
			}
		}
		s.mu.Unlock()
		if err != nil {
			return
		}
	}
}

func (s *Session) handleVoiceReply(reader *lksdk.ByteStreamReader, _ string) {
	data := reader.ReadAll()
	samples, rate, channels, err := audio.DecodeWAV(data)
	index, indexErr := strconv.Atoi(reader.Info.Attributes[roomprotocol.AttrChunkIndex])
	s.mu.Lock()
	defer s.mu.Unlock()
	ex := s.pending[reader.Info.Attributes[roomprotocol.AttrRequestID]]
	if ex == nil {
		return
	}
	if err == nil && (rate <= 0 || channels <= 0 || len(samples) == 0 || indexErr != nil || index < 0) {
		err = fmt.Errorf("invalid voice reply")
	}
	if err != nil {
		s.con.line("(could not play voice reply)")
		ex.signal(err)
		return
	}
	if index < ex.next {
		return
	}
	ex.audio[index] = audio.Resample(audio.DownmixToMono(samples, channels), rate, audio.SampleRate)
	for {
		pcm, ok := ex.audio[ex.next]
		if !ok {
			break
		}
		if s.speaker != nil {
			s.speaker.Push(pcm)
		}
		delete(ex.audio, ex.next)
		ex.next++
	}
	ex.complete()
}

func (s *Session) handleStatus(reader *lksdk.TextStreamReader, _ string) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		var status roomprotocol.MessageStatus
		if json.Unmarshal(scanner.Bytes(), &status) != nil {
			continue
		}
		s.mu.Lock()
		if ex := s.pending[reader.Info.Attributes[roomprotocol.AttrRequestID]]; ex != nil {
			switch status.State {
			case "error":
				s.con.line("(" + status.Message + ")")
				ex.signal(fmt.Errorf("%s", status.Message))
			case "done":
				ex.terminal = true
				ex.expected = status.AudioChunks
				ex.complete()
			default:
				if label := messageProgress(status.State); label != "" {
					s.con.line(label)
				}
			}
		}
		s.mu.Unlock()
	}
}

func messageProgress(state string) string {
	switch state {
	case "queued":
		return "Agent: Message received…"
	case "thinking":
		return "Agent: Thinking…"
	case "transcribing":
		return "Agent: Transcribing your recording…"
	case "audio_ready":
		return "Agent: Reply audio is arriving…"
	}
	return ""
}
