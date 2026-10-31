package room

import (
	"context"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"t10-client/internal/roomprotocol"
	"t10-client/internal/telemetry"
)

var (
	exporter      = tracetest.NewInMemoryExporter()
	installTracer sync.Once
)

type countingSpeaker struct{ pushed int }

func (s *countingSpeaker) Push([]int16) { s.pushed++ }
func (s *countingSpeaker) Drain()       {}

func TestFirstAudioJoinsTheAgentsTraceAndEndsAtTheSpeaker(t *testing.T) {
	// The global tracer can only be pointed at a provider once per process.
	installTracer.Do(func() {
		otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter)))
	})
	exporter.Reset()

	const id = "4bf92f3577b34da6a3ce929d0e0e4736"
	speaker := &countingSpeaker{}
	s := &LiveSession{speaker: speaker}
	ctx := telemetry.ContextWithTraceparent(context.Background(), "00-"+id+"-00f067aa0ba902b7-01")

	s.mu.Lock()
	s.muted = true
	s.awaitFirstAudio(ctx)
	s.mu.Unlock()

	s.playFrame(make([]int16, 960))
	if len(exporter.GetSpans()) != 0 {
		t.Fatal("a frame dropped by mute must not count as first audio")
	}

	s.mu.Lock()
	s.muted = false
	s.mu.Unlock()
	s.playFrame(make([]int16, 960))
	s.playFrame(make([]int16, 960))

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}
	if spans[0].Name != "client.first_audio" || spans[0].SpanContext.TraceID().String() != id {
		t.Errorf("span = %s in trace %s, want client.first_audio in %s", spans[0].Name, spans[0].SpanContext.TraceID(), id)
	}
	if speaker.pushed != 2 {
		t.Errorf("pushed = %d, want 2", speaker.pushed)
	}
}

func TestRequestAttributesStartTheAgentInTheTrace(t *testing.T) {
	ctx, end := telemetry.StartTrace(context.Background(), "test")
	defer end(nil)
	attrs := requestAttributes(ctx, "req-1")
	if attrs[roomprotocol.AttrRequestID] != "req-1" {
		t.Errorf("attrs = %v", attrs)
	}
	if !strings.Contains(attrs[roomprotocol.AttrTraceparent], telemetry.TurnID(ctx)) {
		t.Errorf("traceparent = %q, want turn %s", attrs[roomprotocol.AttrTraceparent], telemetry.TurnID(ctx))
	}
}
