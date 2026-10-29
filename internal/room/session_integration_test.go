package room

import (
	"bytes"
	"context"
	"fmt"
	"github.com/joho/godotenv"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"os"
	"strings"
	"t10-client/internal/audio"
	"t10-client/internal/roomprotocol"
	"testing"
	"time"
)

func TestMessageSessionRoomSmoke(t *testing.T) {
	if os.Getenv("T10_MESSAGE_SMOKE") != "1" {
		t.Skip("set T10_MESSAGE_SMOKE=1 with local LiveKit running")
	}
	env, err := godotenv.Read("../../.env")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{URL: env["LIVEKIT_URL"], APIKey: env["LIVEKIT_API_KEY"], APISecret: env["LIVEKIT_API_SECRET"]}
	roomName := fmt.Sprintf("client-message-smoke-%d", time.Now().UnixNano())
	token, err := mintToken(cfg, "responder", roomName, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	responder, err := lksdk.ConnectToRoomWithToken(cfg.URL, token, lksdk.NewRoomCallback())
	if err != nil {
		t.Fatal(err)
	}
	defer responder.Disconnect()
	failures := make(chan error, 10)
	sendText := func(topic, id, text string) {
		w := responder.LocalParticipant.StreamText(lksdk.StreamTextOptions{Topic: topic, Attributes: map[string]string{roomprotocol.AttrRequestID: id}})
		done := make(chan struct{})
		callback := func() { close(done) }
		w.Write(text, &callback)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			failures <- fmt.Errorf("send text timeout")
		}
		w.Close()
	}
	if err := responder.RegisterTextStreamHandler(roomprotocol.TopicChat, func(r *lksdk.TextStreamReader, _ string) {
		request := r.ReadAll()
		id := r.Info.Attributes[roomprotocol.AttrRequestID]
		if request == "fail" {
			sendText(roomprotocol.TopicMessageStatus, id, "{\"state\":\"error\",\"message\":\"Try again.\"}\n")
			return
		}
		sendText(roomprotocol.TopicMessageStatus, id, "{\"state\":\"queued\"}\n{\"state\":\"done\"}\n")
		sendText(roomprotocol.TopicChat, id, "Complete reply.")
	}); err != nil {
		t.Fatal(err)
	}
	if err := responder.RegisterByteStreamHandler(roomprotocol.TopicVoice, func(r *lksdk.ByteStreamReader, _ string) {
		samples, _, _, err := audio.DecodeWAV(r.ReadAll())
		if err != nil || len(samples) != 3 {
			failures <- fmt.Errorf("upload truncated: %v", err)
			return
		}
		id := r.Info.Attributes[roomprotocol.AttrRequestID]
		// Deliberately deliver terminal status and chunk 1 before chunk 0.
		sendText(roomprotocol.TopicMessageStatus, id, "{\"state\":\"done\",\"audio_chunks\":2}\n")
		for _, index := range []string{"1", "0"} {
			w := responder.LocalParticipant.StreamBytes(lksdk.StreamBytesOptions{Topic: roomprotocol.TopicVoice, Attributes: map[string]string{roomprotocol.AttrRequestID: id, roomprotocol.AttrChunkIndex: index}})
			done := make(chan struct{})
			callback := func() { close(done) }
			w.Write(audio.EncodeWAV([]int16{1, 2}, 24000, 1), &callback)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				failures <- fmt.Errorf("audio send timeout")
			}
			w.Close()
		}
		sendText(roomprotocol.TopicChat, id, "Voice reply.")
		sendText(roomprotocol.TopicTranscript, id, "Heard recording.")
	}); err != nil {
		t.Fatal(err)
	}
	session, err := Connect(context.Background(), cfg, roomName, "caller", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Disconnect()
	var output bytes.Buffer
	session.con = newConsole(&output)
	done := make(chan struct{})
	go func() {
		session.SendText("hello")
		session.SendVoice(audio.EncodeWAV([]int16{1, 2, 3}, 16000, 1))
		session.SendText("fail")
		close(done)
	}()
	select {
	case <-done:
	case err := <-failures:
		t.Fatal(err)
	case <-time.After(10 * time.Second):
		t.Fatal("session did not complete")
	}
	session.con.mu.Lock()
	rendered := output.String()
	session.con.mu.Unlock()
	for _, want := range []string{"Complete reply.", "Voice reply.", "Heard recording.", "Try again."} {
		// Console redraw escapes may separate characters; render the terminal first.
		if !strings.Contains(rendered, want) {
			t.Fatalf("missing %q in output %q", want, rendered)
		}
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if len(session.pending) != 0 {
		t.Fatal("completed exchanges retained")
	}
}
