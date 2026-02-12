// Usage:
//
//	go run examples/web-voice-assistant/test.go
//	open http://localhost:8082

package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/joho/godotenv"
	"github.com/realtime-ai/realtime-ai/pkg/elements"
	"github.com/realtime-ai/realtime-ai/pkg/pipeline"
	"github.com/realtime-ai/realtime-ai/pkg/realtimeapi"
	"github.com/realtime-ai/realtime-ai/pkg/server"
	"github.com/realtime-ai/realtime-ai/pkg/tts"
)

const (
	defaultHTTPPort = ":8082"
	defaultUDPPort  = 9001
)

func main() {
	// Load environment variables
	godotenv.Load()

	// Validate required environment variables
	openaiKey := os.Getenv("OPENAI_API_KEY")
	if openaiKey == "" {
		log.Fatal("OPENAI_API_KEY is required")
	}

	// Find VAD model
	vadModelPath := findVADModel()
	if vadModelPath == "" {
		log.Println("Warning: VAD model not found, interrupt feature will be limited")
	}

	// Get configuration from environment
	httpPort := getEnv("VOICE_ASSISTANT_PORT", defaultHTTPPort)
	udpPort := getEnvInt("VOICE_ASSISTANT_UDP_PORT", defaultUDPPort)
	voice := getEnv("VOICE_ASSISTANT_VOICE", "Coral")
	systemPrompt := getEnv("VOICE_ASSISTANT_SYSTEM_PROMPT",
		"You are a helpful voice assistant. Keep your responses concise, natural, and conversational. Respond in the same language as the user.")

	// Create server configuration
	config := server.DefaultWebRTCRealtimeConfig()
	config.RTCUDPPort = udpPort
	config.ICELite = false

	// Create WebRTC server
	srv := server.NewWebRTCRealtimeServer(config)

	// Set pipeline factory
	srv.SetPipelineFactory(func(ctx context.Context, session *realtimeapi.Session) (*pipeline.Pipeline, error) {
		return createPipeline(ctx, session, PipelineConfig{
			OpenAIKey:     openaiKey,
			VADModelPath:  vadModelPath,
			Voice:         voice,
			SystemPrompt:  systemPrompt,
		})
	})

	// Start WebRTC server
	if err := srv.Start(); err != nil {
		log.Fatalf("Failed to start WebRTC server: %v", err)
	}

	// HTTP handlers
	http.HandleFunc("/session", srv.HandleNegotiate)
	http.Handle("/", http.FileServer(http.Dir("examples/web-voice-assistant")))

	log.Println("===========================================")
	log.Println("  Web Voice Assistant")
	log.Println("===========================================")
	log.Printf("  HTTP: http://localhost%s", httpPort)
	log.Printf("  UDP:  %d", udpPort)
	log.Printf("  Voice: %s", voice)
	log.Printf("  VAD:  %v", vadModelPath != "")
	log.Println("===========================================")

	if err := http.ListenAndServe(httpPort, nil); err != nil {
		log.Fatalf("Failed to start HTTP server: %v", err)
	}
}

// PipelineConfig holds configuration for pipeline creation
type PipelineConfig struct {
	OpenAIKey     string
	VADModelPath  string
	Voice         string
	SystemPrompt  string
}

// createPipeline creates the voice assistant pipeline using OpenAI TTS
func createPipeline(ctx context.Context, session *realtimeapi.Session, cfg PipelineConfig) (*pipeline.Pipeline, error) {
	pipe := pipeline.NewPipeline("voice-assistant-" + session.ID)

	// Enable interrupt manager with hybrid mode
	interruptConfig := pipeline.DefaultInterruptConfig()
	interruptConfig.EnableHybridMode = true
	interruptConfig.MinSpeechForConfirmMs = 300
	interruptConfig.InterruptCooldownMs = 500
	pipe.EnableInterruptManager(interruptConfig)

	// Create elements
	var elems []pipeline.Element
	var prevElem pipeline.Element

	// 1. Input resample: 48kHz → 16kHz (WebRTC to processing)
	inputResample := elements.NewAudioResampleElement(48000, 16000, 1, 1)
	elems = append(elems, inputResample)
	prevElem = inputResample

	// 2. VAD (optional, but recommended for interrupt)
	if cfg.VADModelPath != "" {
		vadConfig := elements.SileroVADConfig{
			ModelPath:       cfg.VADModelPath,
			Threshold:       0.5,
			MinSilenceDurMs: 500,
			SpeechPadMs:     30,
			Mode:            elements.VADModePassthrough,
		}
		vadElem, err := elements.NewSileroVADElement(vadConfig)
		if err != nil {
			log.Printf("[Pipeline] Warning: Failed to create VAD element: %v", err)
		} else {
			if err := vadElem.Init(ctx); err != nil {
				log.Printf("[Pipeline] Warning: Failed to init VAD element: %v", err)
			} else {
				elems = append(elems, vadElem)
				pipe.Link(prevElem, vadElem)
				prevElem = vadElem
				log.Printf("[Pipeline] VAD enabled")
			}
		}
	}

	// 3. Whisper STT
	// asrConfig := elements.WhisperSTTConfig{
	// 	APIKey:               cfg.OpenAIKey,
	// 	Language:             "it",
	// 	Model:                "gpt-4o-transcribe",
	// 	EnablePartialResults: true,
	// 	VADEnabled:           true,
	// 	SampleRate:           16000,
	// 	Channels:             1,
	// }
	// asrElem, err := elements.NewWhisperSTTElement(asrConfig)
	// if err != nil {
	// 	return nil, err
	// }
	
	config := elements.WhisperSTTConfig{
		APIKey:     os.Getenv("GROQ_API_KEY"),
		BaseURL:    "https://api.groq.com/openai/v1", // Endpoint di Groq
		Model:      "whisper-large-v3",                // Modello Groq (molto veloce)
		Language:   "it",
		VADEnabled: true,                              // Consigliato con Groq
	}

	asrElem, err := elements.NewWhisperSTTElement(config)
	if err != nil {
		log.Fatal(err)
	}
	elems = append(elems, asrElem)
	pipe.Link(prevElem, asrElem)
	prevElem = asrElem

	// 4. Chat (OpenAI gpt-4o-mini)
	chatConfig := elements.ChatConfig{
		APIKey:       cfg.OpenAIKey,
		Model:        "gpt-4o-mini",
		SystemPrompt: cfg.SystemPrompt,
		Streaming:    true,
		MaxHistory:   20,
		Temperature:  0.7,
	}
	chatElem, err := elements.NewChatElement(chatConfig)
	if err != nil {
		return nil, err
	}
	elems = append(elems, chatElem)
	pipe.Link(prevElem, chatElem)
	prevElem = chatElem

	// 5. OpenAI TTS (gpt-4o-mini-tts)
	ttsProvider := tts.NewOpenAITTSProvider(cfg.OpenAIKey)
	ttsProvider.SetInstructions("Speak in a friendly and engaging tone in the same user language. Coincise responses.")

	ttsElem := elements.NewUniversalTTSElement(ttsProvider)
	ttsElem.SetVoice("coral")       // scegli la voce: coral, alloy, ash, etc.
	ttsElem.SetLanguage("it-IT")    // lingua
	ttsElem.SetOption("speed", 1.2)   // velocità

	elems = append(elems, ttsElem)
	pipe.Link(prevElem, ttsElem)
	prevElem = ttsElem

	// 6. Nuova soluzione: Simple Resampler manuale
	customResample := NewSimpleResamplerElement()
	elems = append(elems, customResample)
	pipe.Link(prevElem, customResample)
	prevElem = customResample

	// Add all elements to pipeline
	pipe.AddElements(elems)

	log.Printf("[Pipeline] Created voice assistant pipeline for session %s", session.ID)
	log.Printf("[Pipeline] Flow: Resample(48k→16k) → VAD → ASR(11labs) → Chat(gpt-4o-mini) → TTS(OpenAI gpt-4o-mini-tts) → Resample(16k→48k)")

	return pipe, nil
}


// findVADModel looks for the VAD model in common locations
func findVADModel() string {
	paths := []string{
		"models/silero_vad.onnx",
		"../models/silero_vad.onnx",
		"../../models/silero_vad.onnx",
	}

	// Try relative to executable
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		paths = append(paths, filepath.Join(dir, "models", "silero_vad.onnx"))
	}

	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			abs, _ := filepath.Abs(p)
			return abs
		}
	}

	return ""
}

// getEnv returns environment variable value or default
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// getEnvInt returns environment variable as int or default
func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		var result int
		if _, err := os.Stdout.Write([]byte("")); err == nil {
			// Just return default if parsing fails
		}
		if n, err := parseInt(value); err == nil {
			return n
		}
		return result
	}
	return defaultValue
}

func parseInt(s string) (int, error) {
	var n int
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, nil
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

type SimpleResamplerElement struct {
    *pipeline.BaseElement
}

func NewSimpleResamplerElement() *SimpleResamplerElement {
    return &SimpleResamplerElement{
        BaseElement: pipeline.NewBaseElement("simple-resampler", 100),
    }
}

func (e *SimpleResamplerElement) Start(ctx context.Context) error {
    go func() {
        for {
            select {
            case <-ctx.Done():
                return
            case msg := <-e.InChan:
                if msg.Type == pipeline.MsgTypeAudio && len(msg.AudioData.Data) > 0 {
                    // 24k to 48k
                    input := msg.AudioData.Data
                    output := make([]byte, len(input)*2)
                    
                    for i := 0; i < len(input); i += 2 {
                        if i+1 < len(input) {
                            output[i*2] = input[i]
                            output[i*2+1] = input[i+1]
                            output[i*2+2] = input[i]
                            output[i*2+3] = input[i+1]
                        }
                    }
                    
                    msg.AudioData.Data = output
                    msg.AudioData.SampleRate = 48000
                }
                e.OutChan <- msg
            }
        }
    }()
    return nil
}