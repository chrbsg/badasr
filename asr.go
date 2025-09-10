package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"io/ioutil"
	"log"
	"os"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	transcribe "github.com/aws/aws-sdk-go-v2/service/transcribestreaming"
	transcribetypes "github.com/aws/aws-sdk-go-v2/service/transcribestreaming/types"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v3/pkg/media/oggwriter"
	"gopkg.in/hraban/opus.v2"
)

func check(e error) {
	if e != nil {
		panic(e)
	}
}

func ByteToS16(b []byte) []int16 {
	out := make([]int16, len(b)/2)
	for i, _ := range out {
		out[i] = int16(binary.LittleEndian.Uint16(b[i*2 : i*2+2]))
	}
	return out
}

func oggEncoder(pcmChan chan []byte, oggChan chan []byte) {
	defer close(oggChan)
	oggEncoder, err := opus.NewEncoder(48000, 1, opus.AppVoIP)
	check(err)
	sequencer := rtp.NewRandomSequencer()
	payloader := &codecs.OpusPayloader{}
	rtpPacketizer := rtp.NewPacketizer(1200, 0, 0, payloader, sequencer, 48000)
	var oggBuffer bytes.Buffer
	oggWriter, err := oggwriter.NewWith(&oggBuffer, 48000, 1)
	check(err)
	var opus [10000]byte
	fout, err := os.Create("out.ogg")
	check(err)
	defer fout.Close()
	for pcm := range pcmChan {
		s16in := ByteToS16(pcm)
		n, err := oggEncoder.Encode(s16in, opus[:])
		check(err)
		packets := rtpPacketizer.Packetize(opus[:n], 960)
		for _, p := range packets {
			err = oggWriter.WriteRTP(p)
			check(err)
			var txbuf [10000]byte
			n, err := oggBuffer.Read(txbuf[:])
			check(err)
			fout.Write(txbuf[:n])
			oggChan <- txbuf[:n]
		}
	}
}

func amazonASR(oggChan chan []byte) {
	ctx := context.TODO()
	awsCfg, err := config.LoadDefaultConfig(
		ctx,
	)
	check(err)
	client := transcribe.NewFromConfig(awsCfg)

	var sr int32 = 48000
	cfg := &transcribe.StartStreamTranscriptionInput{
		LanguageCode:         transcribetypes.LanguageCode("en-US"),
		MediaEncoding:        transcribetypes.MediaEncodingOggOpus,
		MediaSampleRateHertz: &sr,
	}

	res, err := client.StartStreamTranscription(ctx, cfg)
	check(err)
	stream := res.GetStream()
	defer stream.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for event := range stream.Events() {
			ev, ok := event.(*transcribetypes.TranscriptResultStreamMemberTranscriptEvent)
			if !ok {
				log.Printf("unexpected event %T", ev)
				continue
			}
			for _, res := range ev.Value.Transcript.Results {
				if res.IsPartial || len(res.Alternatives) == 0 {
					continue
				}
				s := *res.Alternatives[0].Transcript
				log.Printf("transcript=%s", s)
			}
		}
	}()

	for ogg := range oggChan {
		ev := &transcribetypes.AudioStreamMemberAudioEvent{
			Value: transcribetypes.AudioEvent{
				AudioChunk: ogg,
			},
		}
		err := stream.Writer.Send(ctx, ev)
		check(err)
	}
	stream.Writer.Close()

	err = stream.Err()
	if err != nil {
		log.Printf("amasr: stream error %v", err)
		return
	}
}

func main() {
	pcm, err := ioutil.ReadFile("dream-short.pcm")
	check(err)
	log.Printf("loaded pcm bytes=%d duration=%dms", len(pcm), 20*len(pcm)/(1920))

	ticker := time.NewTicker(20 * time.Millisecond)
	pcmChan := make(chan []byte)
	oggChan := make(chan []byte)

	go func() {
		i := 0
		for range ticker.C {
			if i+1920 >= len(pcm) {
				return
			}
			pcmChan <- pcm[i : i+1920]
			i += 1920
		}
		close(pcmChan)
	}()

	go oggEncoder(pcmChan, oggChan)

	amazonASR(oggChan)
}
