// Usage:
//
//	go run examples/web-voice-assistant/test.go
//	open http://localhost:8082

package main

import (
    "context"
    "fmt"    // <-- Aggiungi questo
    "log"
    "net/http"
    "os"
    "path/filepath"
    "sync"   // <-- Aggiungi questo
    "time"   // <-- Aggiungi questo

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

type ChatMessage struct {
    Role    string
    Content string
    Time    time.Time
}

func main() {
	// Load environment variables
	godotenv.Load()

	// Validate required environment variables
	openaiKey := os.Getenv("OPENAI_API_KEY")
	if openaiKey == "" {
		log.Fatal("OPENAI_API_KEY is required")
	}

	groqKey := os.Getenv("GROQ_API_KEY")
	if groqKey == "" {
		log.Println("GROQ_API_KEY is not set, using OpenAI API")
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
		"Sei un assistente vocale del servizio clienti di Increso, aiuta i clienti a risolvere i loro problemi. Mantieni le risposte concise, naturali e conversazionali. Rispondi sempre nella stessa lingua dell'utente.")

	// Create server configuration
	config := server.DefaultWebRTCRealtimeConfig()
	config.RTCUDPPort = udpPort
	config.ICELite = false

	// Create WebRTC server
	srv := server.NewWebRTCRealtimeServer(config)

	// Set pipeline factory
	srv.SetPipelineFactory(func(ctx context.Context, session *realtimeapi.Session) (*pipeline.Pipeline, error) {
		// 1. Crea la pipeline
		pipe, err := createPipeline(ctx, session, PipelineConfig{
			OpenAIKey:    openaiKey,
			GroqKey:      groqKey,
			VADModelPath: vadModelPath,
			Voice:        voice,
			SystemPrompt: systemPrompt,
		})
		if err != nil {
			return nil, err
		}

		// 2. Logica per la cronologia
		var history []ChatMessage
		var mu sync.Mutex
		
		// 3. Crea un canale per ricevere gli eventi dal Bus
		eventChan := make(chan pipeline.Event, 100)
		
		// Sottoscrivi agli eventi che ci interessano (User e Assistant usano spesso EventFinalResult)
		pipe.Bus().Subscribe(pipeline.EventFinalResult, eventChan)

		go func() {
			log.Printf("[Logger] Monitoraggio avviato per sessione %s", session.ID)
			for {
				select {
				case <-ctx.Done():
					// Stampa finale quando la sessione chiude
					mu.Lock()
					fmt.Println("\n\n===========================================")
					fmt.Printf(" VERBALE CONVERSAZIONE - %s\n", session.ID)
					fmt.Println("===========================================")
					if len(history) == 0 {
						fmt.Println(" (Nessun messaggio registrato)")
					} else {
						for _, m := range history {
							fmt.Printf("[%s] %-10s: %s\n", m.Time.Format("15:04:05"), m.Role, m.Content)
						}
					}
					fmt.Println("===========================================\n")
					mu.Unlock()
					return

				case ev := <-eventChan:
					// Quando arriva un messaggio finale dal Bus
					mu.Lock()
					text, ok := ev.Payload.(string)
					if ok && text != "" {
						history = append(history, ChatMessage{
							Role:    "MSG",
							Content: text,
							Time:    time.Now(),
						})
					}
					mu.Unlock()
				}
			}
		}()

		return pipe, nil
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
	GroqKey      string
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
	interruptConfig.MinSpeechForConfirmMs = 600
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
			Threshold:       0.85,
			MinSilenceDurMs: 800,
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

	config := elements.WhisperSTTConfig{
		APIKey:     os.Getenv("GROQ_API_KEY"),
		BaseURL:    "https://api.groq.com/openai/v1",
		Model:      "whisper-large-v3",                
		Language:   "it",
		VADEnabled: false,
	}

	asrElem, err := elements.NewWhisperSTTElement(config)
	if err != nil {
		log.Fatal(err)
	}
	elems = append(elems, asrElem)
	pipe.Link(prevElem, asrElem)
	prevElem = asrElem

	// 4. Groq Chat Element
	chatConfig := elements.ChatConfig{
		APIKey:       cfg.GroqKey,                      // Usa chiave Groq
		BaseURL:      "https://api.groq.com/openai/v1", // Endpoint Groq
		Model:        "openai/gpt-oss-20b",        		// Modello Groq raccomandato
		SystemPrompt: cfg.SystemPrompt,
		Streaming:    true,
		MaxHistory:   20,
		Temperature:  0.4,
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

	ttsElem.SetVoice("marin")         // scegli la voce: coral, alloy, ash, etc.
	ttsElem.SetLanguage("it-IT")      // lingua
	ttsElem.SetOption("speed", 1)     // velocità
	ttsElem.SetOption("format", "pcm")

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