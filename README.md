# mac-client

A real interactive LiveKit participant for the [t10
agent](../livekit-agent): it captures your default microphone, publishes it as an
Opus audio track, and plays whatever audio track the agent publishes back
through your default speaker — while also printing the live STT transcript
and the LLM reply as they stream in. It's the mic/speaker counterpart to
the agent's `cmd/testclient live`, which only simulates a call from a WAV
file.

## Prerequisites

- `libopus`, `libopusfile`, and `pkg-config` (cgo binding to libopus, same
  as the agent needs): `brew install opus opusfile pkgconf` on macOS.
- No extra system audio library — microphone capture and speaker playback
  go through [`malgo`](https://github.com/gen2brain/malgo) (miniaudio),
  which links your OS's native audio API (CoreAudio on macOS) directly via
  cgo.
- A microphone and speakers/headphones, and OS permission for your
  terminal to access the microphone (macOS will prompt on first run).

## Running

Start the agent stack first (from `../livekit-agent`):

```bash
cd ../livekit-agent
cp .env.example .env   # fill in DASHSCOPE_API_KEY, OPENAI_*
docker compose up --build
```

On installations without the Compose CLI plugin, use `docker-compose` in
place of `docker compose`. The configured host LiveKit URL is
`ws://localhost:8890`; match it in the client `.env`.

Then, in this directory:

```bash
cp .env.example .env
go build -o t10-client ./cmd/client

# Option A: Connect using Auth Service Room Admission (recommended production pattern)
# Signs in through the browser and keeps the session in the Keychain.
./t10-client -auth-url http://localhost:8081 -room voice-agent-demo

# End that session:
./t10-client -auth-url http://localhost:8081 -logout

# With a password, against a dev-mode auth service only:
./t10-client -auth-url http://localhost:8081 -username ada@acme.example -password change-me-enterprise-admin -room voice-agent-demo

# Or with an existing Bearer token:
./t10-client -auth-url http://localhost:8081 -token "$AUTH_TOKEN" -room voice-agent-demo

# Option B: Local Token Minting (explicit development mode with LIVEKIT_API_SECRET)
./t10-client -mode live -room voice-agent-demo
```

Rebuild the executable after changing source; an already running process keeps
using the old binary until you stop and restart it. Live mode is the default.
Use `-mode text` or `-mode voice` for the existing message-based modes.

Speak into your microphone. A `You: [listening]` line appears the moment the agent
hears you and fills in word by word as it recognizes you, then snaps to the
final transcript when you stop. `Agent: ...` follows, arriving in step with
the voice coming out of your speakers rather than racing ahead of it — what
is on screen is what has actually been said.

Interrupt whenever you like: the agent stops mid-word, its line is closed
with an `[interrupted]` label where it got to, and your next `You:` line starts underneath.
Ctrl+C to disconnect. After two seconds the log reports `microphone audio
flowing` and `peak_frame_rms`; a value near zero means the capture device is
sending silence, even if the room connected successfully.

To also send traces/metrics/logs to the agent stack's observability container
(`otel-lgtm`, brought up by the same `docker compose up`), uncomment
`OTEL_EXPORTER_OTLP_ENDPOINT` in `.env` (points to `localhost:4317`) —
see `.env.example` and `CLAUDE.md`'s "Observability" section (UI at http://localhost:3002).

## How it fits together

```
mac-client/
  cmd/client/main.go         # CLI entrypoint: opens audio devices, connects, blocks on Ctrl+C
  internal/audio/            # malgo mic capture + speaker playback, 48kHz mono PCM16
  internal/audio/opuscodec/        # cgo Opus encode/decode (RTP framing: 48kHz/20ms/960 samples)
  internal/roomprotocol/     # topic and stream-attribute contract constants
  internal/room/             # LiveKit token+connect, publish mic track, subscribe agent track
  internal/telemetry/        # session/turn IDs + OTel spans/metrics (mirrors the agent's own)
```

`internal/room` also registers text-stream handlers on `agent.transcript`
and `agent.chat` — the same two topics the agent's text/voice modes use —
plus `agent.transcript.interim` (the live caption of what you're saying) and
`agent.state`. That last one is what makes an interruption stop cleanly:
audio already on the wire is buffered in this process and the agent can't
reach it, so the agent's `interrupted` message tells this side to drop it
too. Everything printed goes through `internal/room/console.go`, which owns a
single live line at the bottom of the screen.

Capture and playback both run at 48kHz mono — Opus's fixed RTP clock rate —
so there's no resampling step anywhere. Both the mic's capture callback and
the speaker's playback callback come from the sound card itself, which is
what paces publishing and playback in real time; contrast the agent's own
`internal/livekit` transport adapter, which has to drive an RTP track with
a manual `time.Ticker` because nothing else supplies that pacing on its
side.

This is a separate Go module from `../livekit-agent` (a deliberate choice — see the
workspace root's README), so it can't import the agent's `internal/`
packages; the small amount of overlapping logic (Opus framing constants,
LiveKit token minting) is duplicated rather than shared.

## Verifying the live path without microphone permissions

Run an agent. It joins any room with someone in it, so use a test LiveKit server
rather than a second agent on the one normal calls use:

```bash
cd ../livekit-agent
go run ./cmd/agent -identity ux-agent
```

Then run the client's opt-in integration test with a spoken question WAV:

```bash
T10_LIVE_SMOKE_WAV=/absolute/path/question.wav go test -race ./internal/room -run TestLiveSessionProviderSmoke -count=1 -v
```

The test uses the normal live client's track and caption handlers and requires
a visible interim transcript before the final, exactly one rendered agent
reply, decoded speech audio, and an idle transition.
Run it alone in that room, since all participants subscribe to the room's
audio tracks. It verifies the provider and transport path; it does not test
macOS microphone permissions or physical speaker output.

Live captions stay in a single bounded preview row while speech is in progress;
completed lines are printed once in full. `[thinking]` indicates the silent
wait for an answer. A `‹` at the start of a long preview means earlier words
are outside that row, not that a second reply was generated.

Live mode writes diagnostic/SDK logs to `t10-client.log` by default so they
cannot corrupt the caption row. Use `tail -f t10-client.log` in another terminal
to inspect state messages and microphone diagnostics. `-log-file` selects a
different file; an empty value restores stderr logging.


Text and voice messages show receipt/progress and explicit failures. Voice replies
arrive as ordered WAV chunks and begin playing before the complete answer is
ready. The client correlates replies by request ID and waits for all advertised
chunks before completing the exchange. Run matching livekit-agent/mac-client versions: the
message protocol now requires `agent.message.status` and chunk attributes.

## License

This project is licensed under the [MIT License](LICENSE).

