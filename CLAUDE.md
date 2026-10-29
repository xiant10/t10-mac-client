# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

This is the **mac-client** subproject of the `t10` workspace — an
independent Go module and git repo, separate from the sibling `livekit-agent/`
repo (see its own `CLAUDE.md`). It supports interactive text, recorded voice messages, and live calls. In live
mode it is a LiveKit participant: it captures the default microphone, publishes it as an Opus
audio track, and plays back whatever audio track the agent publishes to
the room. See `README.md` for prerequisites and how to run it against the
agent's `docker-compose.yml` LiveKit dev server.

Being a separate module means it cannot import the agent's `internal/`
packages — the small amount of overlapping logic (Opus RTP framing
constants in `internal/audio/opuscodec`, LiveKit token minting in
`internal/room/session.go`, and the whole of `internal/telemetry`) is
local to each module. Room contract constants live in internal/roomprotocol;
the workspace architecture check verifies their parity. Future public libraries
may be versioned deliberately; separate modules do not prohibit sharing public APIs.

## Commands

```bash
go build ./...   # build everything
go vet ./...     # vet
go test ./...    # run all tests (terminal rendering; no audio device needed)
# Run with Auth Service room admission (production pattern):
go run ./cmd/client -auth-url http://localhost:8081 -username ada@acme.example -password change-me-enterprise-admin -room voice-agent-demo
# Or in local dev mode (requires LIVEKIT_API_SECRET in .env):
go run ./cmd/client -room voice-agent-demo -identity mic-client
```

Requires `libopus`, `libopusfile`, and `pkg-config` on the host (same cgo
Opus binding the agent uses): `brew install opus opusfile pkgconf` on
macOS. Microphone/speaker I/O itself needs no extra system library — it
goes through `malgo` (miniaudio), which links the OS's native audio API
directly.

## Architecture

- **`internal/audio/opuscodec`** — cgo wrapper around libopus, fixed 48kHz/20ms/
  960-sample framing to match a LiveKit audio track's RTP clock. Copied
  from (and kept in sync by hand with) the agent's own
  `internal/audio/opuscodec`.
- **`internal/audio`** — `Mic` (malgo capture device → channel of 960-
  sample PCM16 frames) and `Speaker` (malgo playback device ← queued PCM16,
  silence-fills on underrun). Both run at 48kHz mono, so there's no
  resampling anywhere in this module. The sound card's own capture/playback
  callbacks are the real-time pacing source on both sides — no
  `time.Ticker` needed, unlike the agent's `internal/livekit` transport
  adapter, which has to invent pacing for an RTP track with nothing else
  driving it.
- **`internal/room`** — acquires LiveKit room credentials (either from the central auth service's `POST /v1/rooms/admission` or via local development token minting), connects, publishes the mic as an Opus track (if permitted by `can_publish`), and (via `OnTrackSubscribed`) decodes+plays back whatever track the agent publishes. Also registers text-stream handlers on four topics (duplicated topic-name constants — see the agent's `internal/roomprotocol`): the live interim caption, the final transcript, the agent's reply, and the agent's call state.

  **`agent.state` is not cosmetic.** Its `interrupted` message is how this
  process learns a barge-in happened, and it is the only way the queued audio
  in `Speaker`'s own buffer ever gets dropped — the agent draining its side
  cannot reach audio already on the wire. Without it an interruption trails
  off over the top of whoever is talking. It arrives over the data channel,
  ahead of the audio it invalidates.
- **`internal/room/console.go`** — everything printed to the terminal goes
  through one `console`, which owns a single live line at the bottom of the
  screen and hands ownership between the caller's caption and the agent's
  reply. It replaced a scheme that made each stream wait for the previous one
  to finish printing: that kept lines in order, but since interim captioning
  starts the moment you speak and the gate ahead of it was the *previous
  reply* finishing, you got no feedback that you were being heard for exactly
  as long as the agent was talking — which is precisely when you'd be
  interrupting. Nothing waits here; whoever produces text takes the line and
  commits the previous owner's. `console_test.go` asserts against the lines a
  terminal would actually be showing, escape sequences replayed.
- **`internal/roomprotocol`** — SDK-free room topic and stream-attribute constants;
  workspace checks compare them with the agent copy.
- **`cmd/client`** — CLI entrypoint wiring the above together; blocks on
  `Ctrl+C` for clean shutdown.
- **`internal/telemetry`** — a trimmed copy of the agent's own package of
  the same name (see `../livekit-agent/CLAUDE.md`'s "Correlation IDs, tracing, and
  metrics" section for the full design): `StartSession` stamps a
  correlation ID and opens a root span per run, `StartStage` times
  connect/publish/playback stages as child spans + metrics, both no-ops
  until `SetupOTel` installs a real OTLP pipeline. `cmd/client/main.go`
  calls `SetupOTel` when `OTEL_EXPORTER_OTLP_ENDPOINT` is set (see
  `.env.example`) and wraps the default slog handler in
  `telemetry.NewLogHandler` so every log line carries `correlation_id`.

## Observability

Run the agent stack's `docker compose up` first (from `../livekit-agent`) — it
brings up `otel-lgtm` (unified Grafana UI at http://localhost:3002 for
Traces, Metrics, and Logs) alongside the agent itself, with OTLP/gRPC port
4317 published to the host. Point this client's
`OTEL_EXPORTER_OTLP_ENDPOINT` (in `.env`) at `localhost:4317` to export
`internal/room.Connect`'s stage spans (`room.mint_token`, `room.connect`,
`room.publish_track`, `playback.time_to_first_agent_audio`), stage metrics,
and slog logs there. A client-observed trace and the agent-side spans that
produced it are separate traces today (no trace context is propagated over
the LiveKit room) — see `../livekit-agent/docs/DESIGN.md` §7 for why, and what
unifying them would take.

## Known constraint when testing here

Actually exercising microphone capture and speaker playback needs a real
terminal with OS audio-device permissions (macOS prompts for mic access on
first run) — this can't be verified by an agent running in a sandboxed/
headless environment. `go build ./...`, `go vet ./...`, and `go test ./...`
(which covers the terminal rendering in `internal/room/console_test.go`, the
one part of the call UI that needs no audio at all) are the available
automated checks; treat those as "compiles cleanly and renders correctly,"
not "confirmed working audio," and say so explicitly rather than claiming the
mic/speaker path was manually verified.
