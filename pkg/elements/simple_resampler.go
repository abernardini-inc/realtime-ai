package elements

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/realtime-ai/realtime-ai/pkg/pipeline"
)

// SimpleResampler è un elemento ottimizzato specificamente per OpenAI TTS -> WebRTC.
// Non usa FFmpeg. Converte 24kHz Mono -> 48kHz Mono e frammenta i pacchetti.
type SimpleResampler struct {
	*pipeline.BaseElement
	wg sync.WaitGroup
}

// Costante: 20ms di audio a 48kHz (16-bit mono) = 1920 bytes
// (48000 campioni/s * 2 bytes/campione * 0.020 s)
const webRTCFrameSize = 1920

func NewSimpleResampler() *SimpleResampler {
	return &SimpleResampler{
		BaseElement: pipeline.NewBaseElement("simple-resampler", 100),
	}
}

func (e *SimpleResampler) Start(ctx context.Context) error {
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		log.Println("[SimpleResampler] Started: Optimized 24k->48k converter")

		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-e.BaseElement.InChan:
				if !ok {
					return
				}

				// Se non è audio, passalo e basta
				if msg.Type != pipeline.MsgTypeAudio || msg.AudioData == nil {
					e.BaseElement.OutChan <- msg
					continue
				}

				inputData := msg.AudioData.Data
				if len(inputData) == 0 {
					continue
				}

				// 1. UPSAMPLING (24k -> 48k)
				// Raddoppiamo i dati: ogni 2 byte (1 campione) vengono copiati due volte.
				upsampledData := make([]byte, len(inputData)*2)
				for i := 0; i < len(inputData); i += 2 {
					if i+1 >= len(inputData) {
						break
					}
					// Copia il campione due volte
					upsampledData[i*2] = inputData[i]
					upsampledData[i*2+1] = inputData[i+1]
					upsampledData[i*2+2] = inputData[i]
					upsampledData[i*2+3] = inputData[i+1]
				}

				// 2. CHUNKING (Invio a pacchetti piccoli per WebRTC)
				// OpenAI manda blocchi enormi (es. 100KB). WebRTC ne vuole ~1200 bytes.
				// Spezziamo l'audio in frame da 20ms (1920 bytes).
				for i := 0; i < len(upsampledData); i += webRTCFrameSize {
					end := i + webRTCFrameSize
					if end > len(upsampledData) {
						end = len(upsampledData)
					}
					
					chunk := upsampledData[i:end]

					// Invia solo se abbiamo dati
					if len(chunk) > 0 {
						outMsg := &pipeline.PipelineMessage{
							Type:      pipeline.MsgTypeAudio,
							SessionID: msg.SessionID,
							Timestamp: time.Now(),
							AudioData: &pipeline.AudioData{
								Data:       chunk,
								SampleRate: 48000, // Ora siamo a 48k
								Channels:   1,     // Mono
								MediaType:  pipeline.AudioMediaTypeRaw,
								Timestamp:  time.Now(),
							},
						}

						select {
						case e.BaseElement.OutChan <- outMsg:
						case <-ctx.Done():
							return
						}
						
						// Opzionale: Piccola pausa per non inondare il buffer UDP se il blocco è enorme
						// time.Sleep(time.Microsecond * 500) 
					}
				}
			}
		}
	}()
	return nil
}

func (e *SimpleResampler) Stop() error {
	e.wg.Wait()
	return nil
}