package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"
	"encoding/binary"
    "os/exec"

	"github.com/joho/godotenv"
	"github.com/realtime-ai/realtime-ai/pkg/elements"
	"github.com/realtime-ai/realtime-ai/pkg/pipeline"
	"github.com/realtime-ai/realtime-ai/pkg/tts"
)

func playAudioPCM(audio *pipeline.AudioData) error {
    // Creiamo un file WAV
    f, err := os.Create("output.wav")
    if err != nil {
        return err
    }
    defer f.Close()

    // Header WAV semplice (16 bit PCM)
    // Formato minimale: 16-bit PCM, stereo o mono
    numChannels := audio.Channels
    sampleRate := audio.SampleRate
    bitsPerSample := 16
    byteRate := sampleRate * numChannels * bitsPerSample / 8
    blockAlign := numChannels * bitsPerSample / 8
    dataSize := len(audio.Data)

    // Scrive header WAV
    f.Write([]byte("RIFF"))
    binary.Write(f, binary.LittleEndian, uint32(36+dataSize))
    f.Write([]byte("WAVEfmt "))
    binary.Write(f, binary.LittleEndian, uint32(16)) // PCM header size
    binary.Write(f, binary.LittleEndian, uint16(1))  // Audio format PCM
    binary.Write(f, binary.LittleEndian, uint16(numChannels))
    binary.Write(f, binary.LittleEndian, uint32(sampleRate))
    binary.Write(f, binary.LittleEndian, uint32(byteRate))
    binary.Write(f, binary.LittleEndian, uint16(blockAlign))
    binary.Write(f, binary.LittleEndian, uint16(bitsPerSample))
    f.Write([]byte("data"))
    binary.Write(f, binary.LittleEndian, uint32(dataSize))
    f.Write(audio.Data)

    // Riproduce con player di sistema (modifica in base al tuo OS)
    cmd := exec.Command("ffplay", "-autoexit", "output.wav")
    return cmd.Run()
}


func main() {
	// Load environment variables from .env file
	godotenv.Load()

	// Check for API key
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		log.Fatal("OPENAI_API_KEY environment variable is not set")
	}

	// Create OpenAI TTS provider (gpt-4o-mini-tts model)
	provider := tts.NewOpenAITTSProvider(apiKey)

	// Set voice instructions for controlling tone/style
	provider.SetInstructions("Speak in a friendly and engaging tone")

	// Create TTS element with the provider
	ttsElement := elements.NewUniversalTTSElement(provider)

	// Configure voice
	// Available voices: alloy, ash, ballad, coral, echo, fable, nova, onyx, sage, shimmer, verse, marin, cedar
	// Recommended: marin, cedar (highest quality)
	ttsElement.SetVoice("coral")

	// Set language (optional)
	ttsElement.SetLanguage("en-US")

	// Set additional options (optional)
	// Speed: 0.25 to 4.0, default 1.0
	ttsElement.SetOption("speed", 1.0)
	ttsElement.SetOption("format", "pcm") // pcm, opus, mp3, wav

	// Create pipeline
	p := pipeline.NewPipeline("openai-tts-demo")
	p.AddElement(ttsElement)

	// Start the pipeline
	ctx := context.Background()
	if err := ttsElement.Start(ctx); err != nil {
		log.Fatalf("Failed to start TTS element: %v", err)
	}
	defer ttsElement.Stop()

	// Example: Synthesize some text
	texts := []string{
		"Hello! This is a test of OpenAI's gpt-4o-mini-tts model.",
		"I'm using the Coral voice, which sounds natural and conversational.",
		"The voice instructions help control the tone and style of speech.",
	}

	for i, text := range texts {
		fmt.Printf("\n[%d] Synthesizing: %s\n", i+1, text)

		// Create text message
		msg := &pipeline.PipelineMessage{
			Type: pipeline.MsgTypeData,
			TextData: &pipeline.TextData{
				Data:      []byte(text),
				Timestamp: time.Now(),
			},
		}

		// Send to TTS element
		ttsElement.In() <- msg

		// Receive audio output
		select {
		case audioMsg := <-ttsElement.Out():
			if audioMsg.AudioData != nil {
				fmt.Printf("Received audio: %d bytes\n", len(audioMsg.AudioData.Data))
				if err := playAudioPCM(audioMsg.AudioData); err != nil {
					log.Printf("Error playing audio: %v", err)
				}
			}
		case <-time.After(10 * time.Second):
			log.Printf("Timeout waiting for audio output")
		}

		time.Sleep(100 * time.Millisecond)
	}

	fmt.Println("\nDemo completed successfully!")

	// Example: List supported voices
	fmt.Println("\nSupported voices:")
	for _, voice := range ttsElement.GetSupportedVoices() {
		fmt.Printf("  - %s\n", voice)
	}
}
