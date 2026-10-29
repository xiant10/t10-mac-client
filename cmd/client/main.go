// Command client runs a continuous live call by default. The text and voice
// modes retain the message REPL and one-shot recording workflow.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/gen2brain/malgo"
	"github.com/joho/godotenv"

	"t10-client/internal/audio"
	"t10-client/internal/room"
	"t10-client/internal/telemetry"
)

// setupLogging configures slog output based on the mode and log path.
// Returns the opened log file (or nil if no file logging), which the caller
// should defer-close.
func setupLogging(mode, logPath, logLevel string) *os.File {
	logOutput := io.Writer(os.Stderr)
	var file *os.File
	if logPath != "" {
		var err error
		file, err = os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			fmt.Fprintf(os.Stderr, "open diagnostic log: %v\n", err)
			os.Exit(1)
		}
		if mode == "live" {
			logOutput = file
		} else {
			logOutput = io.MultiWriter(os.Stderr, file)
		}
		log.SetOutput(file)
	}
	baseHandler := slog.NewTextHandler(logOutput, &slog.HandlerOptions{Level: parseLogLevel(logLevel)})
	slog.SetDefault(slog.New(telemetry.NewOTelLogHandler(telemetry.NewLogHandler(baseHandler))))
	return file
}

// setupOTel initializes OpenTelemetry export if OTEL_EXPORTER_OTLP_ENDPOINT
// is set. Returns a shutdown function (may be nil if OTel is not configured).
func setupOTel(ctx context.Context) func(context.Context) error {
	endpoint := getenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	if endpoint == "" {
		return nil
	}
	shutdown, err := telemetry.SetupOTel(ctx, "t10-client", endpoint)
	if err != nil {
		slog.Error("otel setup failed; continuing without trace/metric export", "endpoint", endpoint, "err", err)
		return nil
	}
	slog.Info("otel export enabled", "endpoint", endpoint)
	return shutdown
}

// initAudio initializes the audio context, speaker, and microphone.
// Returns a cleanup function that uninitializes and frees all resources.
func initAudio(ctx context.Context) (*audio.Speaker, *audio.Mic, func()) {
	audioCtx, err := malgo.InitContext(nil, malgo.ContextConfig{}, func(string) {})
	if err != nil {
		fatal(ctx, "init audio context", err)
	}

	speaker, err := audio.NewSpeaker(audioCtx)
	if err != nil {
		fatal(ctx, "open speaker", err)
	}
	if err := speaker.Start(); err != nil {
		fatal(ctx, "start speaker", err)
	}

	mic, err := audio.NewMic(audioCtx)
	if err != nil {
		fatal(ctx, "open microphone", err)
	}
	if err := mic.Start(); err != nil {
		fatal(ctx, "start microphone", err)
	}

	cleanup := func() {
		mic.Stop()
		speaker.Stop()
		_ = audioCtx.Uninit()
		audioCtx.Free()
	}
	return speaker, mic, cleanup
}

func main() {
	_ = godotenv.Load()

	roomName := flag.String("room", "voice-agent-demo", "LiveKit room to join")
	identity := flag.String("identity", "mic-client", "participant identity")
	startMode := flag.String("mode", "live", "starting mode: live, text, or voice")
	logPath := flag.String("log-file", "t10-client.log", "live-call diagnostic log (empty sends logs to stderr)")
	authURL := flag.String("auth-url", "", "Auth service base URL (e.g. http://localhost:8081, env: AUTH_URL)")
	authToken := flag.String("token", "", "Auth bearer token for room admission (env: AUTH_TOKEN)")
	tenant := flag.String("tenant", "", "Tenant identifier for room admission (env: AUTH_TENANT)")
	username := flag.String("username", "", "Username for login to auth service (env: AUTH_USERNAME)")
	password := flag.String("password", "", "Password for login to auth service (env: AUTH_PASSWORD)")
	flag.Parse()

	if *startMode != "live" && *startMode != "text" && *startMode != "voice" {
		fmt.Fprintf(os.Stderr, "-mode must be live, text, or voice, got %q\n", *startMode)
		os.Exit(1)
	}

	if f := setupLogging(*startMode, *logPath, getenv("LOG_LEVEL", "info")); f != nil {
		defer f.Close()
	}

	ctx, end := telemetry.StartSession(context.Background())
	defer end()

	if shutdown := setupOTel(ctx); shutdown != nil {
		otelShutdown = shutdown
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := shutdown(shutdownCtx); err != nil {
				slog.Error("otel shutdown failed", "err", err)
			}
		}()
	}

	resolvedAuthURL := *authURL
	if resolvedAuthURL == "" {
		resolvedAuthURL = getenv("AUTH_URL", "")
	}
	resolvedAuthToken := *authToken
	if resolvedAuthToken == "" {
		resolvedAuthToken = getenv("AUTH_TOKEN", "")
	}
	resolvedTenant := *tenant
	if resolvedTenant == "" {
		resolvedTenant = getenv("AUTH_TENANT", "")
	}
	resolvedUsername := *username
	if resolvedUsername == "" {
		resolvedUsername = getenv("AUTH_USERNAME", "")
	}
	resolvedPassword := *password
	if resolvedPassword == "" {
		resolvedPassword = getenv("AUTH_PASSWORD", "")
	}

	cfg := room.Config{
		URL:       getenv("LIVEKIT_URL", "ws://localhost:8890"),
		APIKey:    getenv("LIVEKIT_API_KEY", "devkey"),
		APISecret: getenv("LIVEKIT_API_SECRET", ""),
		AuthURL:   resolvedAuthURL,
		AuthToken: resolvedAuthToken,
		Tenant:    resolvedTenant,
		Username:  resolvedUsername,
		Password:  resolvedPassword,
	}

	speaker, mic, audioCleanup := initAudio(ctx)
	defer audioCleanup()

	if *startMode == "live" {
		callCtx, stop := signal.NotifyContext(ctx, os.Interrupt)
		defer stop()
		sess, err := room.ConnectLive(callCtx, cfg, *roomName, *identity, mic.Frames(), speaker)
		if err != nil {
			fatal(ctx, "connect live call", err)
		}
		defer sess.Disconnect()
		fmt.Println("Live call connected. Speak naturally; interrupt whenever you like. Ctrl+C to end.")
		if *logPath != "" {
			fmt.Println("Diagnostics:", *logPath)
		}
		<-callCtx.Done()
		return
	}
	rec := audio.NewRecorder(mic.Frames())

	sess, err := room.Connect(ctx, cfg, *roomName, *identity, speaker)
	if err != nil {
		fatal(ctx, "connect", err)
	}
	defer sess.Disconnect()

	slog.InfoContext(ctx, "connected", "identity", *identity, "room", *roomName, "livekit_url", cfg.URL)

	runREPL(*startMode, sess, rec)
	slog.InfoContext(ctx, "shutting down")
}

// runREPL reads lines from stdin, sending each as a text chat message (in
// text mode) or using it to bracket a voice recording (in voice mode), until
// stdin closes, /quit is typed, or Ctrl+C arrives. It reads from a
// background goroutine and selects against the interrupt signal, rather
// than blocking directly on a bufio.Scanner in the main goroutine, so Ctrl+C
// is honored promptly even while waiting for the next line — including
// mid-recording, where it aborts and discards the take.
func runREPL(mode string, sess *room.Session, rec *audio.Recorder) {
	lines := make(chan string)
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)

	printHelp()
	printPrompt(mode)
	for {
		select {
		case <-stop:
			fmt.Println()
			return
		case line, ok := <-lines:
			if !ok {
				return
			}
			switch strings.TrimSpace(line) {
			case "/quit", "/exit":
				return
			case "/help":
				printHelp()
				printPrompt(mode)
				continue
			case "/text":
				mode = "text"
				printPrompt(mode)
				continue
			case "/voice":
				mode = "voice"
				printPrompt(mode)
				continue
			}

			if mode == "voice" {
				fmt.Println("recording... press Enter to stop and send")
				rec.Start()
				select {
				case <-stop:
					rec.Stop()
					return
				case _, ok := <-lines:
					if !ok {
						rec.Stop()
						return
					}
					if samples := rec.Stop(); len(samples) == 0 {
						fmt.Println("(nothing recorded)")
					} else {
						sess.SendVoice(audio.EncodeWAV(samples, audio.SampleRate, audio.Channels))
					}
				}
			} else if text := strings.TrimSpace(line); text != "" {
				sess.SendText(text)
			}
			printPrompt(mode)
		}
	}
}

func printHelp() {
	fmt.Println("type a message to chat; /voice for voice messages, /text to switch back, /quit to exit")
}

func printPrompt(mode string) {
	if mode == "voice" {
		fmt.Print("[voice] press Enter to record> ")
	} else {
		fmt.Print("[text]> ")
	}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

var otelShutdown func(context.Context) error

func fatal(ctx context.Context, what string, err error) {
	fmt.Fprintf(os.Stderr, "%s: %v\n", what, err)
	slog.ErrorContext(ctx, "fatal", "what", what, "err", err)
	if otelShutdown != nil {
		flushCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = otelShutdown(flushCtx)
	}
	os.Exit(1)
}

func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
