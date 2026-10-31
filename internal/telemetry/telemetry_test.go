package telemetry

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

var (
	exporter      = tracetest.NewInMemoryExporter()
	installTracer sync.Once
)

// These run first (source order): they cover the no-SDK path, and the global
// provider below can only be installed once.

func TestStartTraceWithoutAnSDK(t *testing.T) {
	if got := TurnID(context.Background()); got != "" {
		t.Fatalf("TurnID on a bare context = %q", got)
	}
	ctx, end := StartTrace(context.Background(), "test")
	defer end(nil)
	id := TurnID(ctx)
	if len(id) != 32 {
		t.Fatalf("TurnID = %q, want a 32-char hex trace ID", id)
	}
	stageCtx, stageEnd := StartStage(ctx, "test.stage")
	defer stageEnd(nil)
	if got := TurnID(stageCtx); got != id {
		t.Errorf("TurnID inside a stage = %q, want %q", got, id)
	}
	if got := Traceparent(ctx); !strings.Contains(got, id) {
		t.Errorf("Traceparent = %q, want it to contain %s", got, id)
	}
	ctx2, end2 := StartTrace(context.Background(), "test")
	defer end2(nil)
	if TurnID(ctx2) == id {
		t.Error("two traces produced the same ID")
	}
}

func TestContextWithTraceparent(t *testing.T) {
	const id = "4bf92f3577b34da6a3ce929d0e0e4736"
	ctx := ContextWithTraceparent(context.Background(), "00-"+id+"-00f067aa0ba902b7-01")
	if got := TurnID(ctx); got != id {
		t.Errorf("TurnID = %q, want %q", got, id)
	}
	if got := TurnID(ContextWithTraceparent(context.Background(), "")); got != "" {
		t.Errorf("TurnID with no traceparent = %q", got)
	}
}

func TestLogHandlerAttachesIDs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(NewLogHandler(slog.NewTextHandler(&buf, nil)))

	logger.InfoContext(context.Background(), "nothing")
	if strings.Contains(buf.String(), "_id=") {
		t.Errorf("log line outside a session has IDs: %s", buf.String())
	}
	buf.Reset()

	ctx, end := StartTrace(WithSessionID(context.Background(), "PA_1"), "test")
	defer end(nil)
	logger.InfoContext(ctx, "in trace")
	if want := "turn_id=" + TurnID(ctx); !strings.Contains(buf.String(), want) || !strings.Contains(buf.String(), "session_id=PA_1") {
		t.Errorf("log line = %q, want session_id and %s", buf.String(), want)
	}
}

func TestLogHandlerReachesBothBranchesOfTheOTelFanout(t *testing.T) {
	var local, bridged bytes.Buffer
	logger := slog.New(NewLogHandler(&fanoutHandler{local: slog.NewTextHandler(&local, nil), otel: slog.NewTextHandler(&bridged, nil)}))

	logger.InfoContext(WithSessionID(context.Background(), "PA_1"), "line")
	for name, buf := range map[string]*bytes.Buffer{"local": &local, "otel": &bridged} {
		if !strings.Contains(buf.String(), "session_id=PA_1") {
			t.Errorf("%s branch = %q, want session_id", name, buf.String())
		}
	}
}

func TestSpans(t *testing.T) {
	installTracer.Do(func() {
		otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter)))
	})
	exporter.Reset()

	ctx, end := StartTrace(WithSessionID(context.Background(), "PA_1"), "root")
	_, stageEnd := StartStage(ctx, "stage")
	stageEnd(nil)
	end(nil)

	spans := exporter.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("spans = %d, want 2", len(spans))
	}
	stage, root := spans[0], spans[1]
	if root.Parent.IsValid() {
		t.Error("StartTrace should make a true root")
	}
	if root.SpanContext.TraceID().String() != TurnID(ctx) {
		t.Error("TurnID is not the trace ID actually exported")
	}
	if !stage.Parent.Equal(root.SpanContext) {
		t.Error("stage is not a child of the root")
	}
	for _, s := range []tracetest.SpanStub{root, stage} {
		found := false
		for _, kv := range s.Attributes {
			found = found || (kv.Key == "session.id" && kv.Value.AsString() == "PA_1")
		}
		if !found {
			t.Errorf("%s has no session.id: %v", s.Name, s.Attributes)
		}
	}
}
