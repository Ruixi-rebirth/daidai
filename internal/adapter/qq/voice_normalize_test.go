package qq

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestNormalizeVoiceAudio(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("FFmpeg is unavailable")
	}
	// A tenth of a second of silent, mono PCM audio at 16 kHz.
	wav := make([]byte, 44+3200)
	copy(wav, "RIFF")
	binary.LittleEndian.PutUint32(wav[4:], uint32(len(wav)-8))
	copy(wav[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(wav[16:], 16)
	binary.LittleEndian.PutUint16(wav[20:], 1)
	binary.LittleEndian.PutUint16(wav[22:], 1)
	binary.LittleEndian.PutUint32(wav[24:], 16000)
	binary.LittleEndian.PutUint32(wav[28:], 32000)
	binary.LittleEndian.PutUint16(wav[32:], 2)
	binary.LittleEndian.PutUint16(wav[34:], 16)
	copy(wav[36:], "data")
	binary.LittleEndian.PutUint32(wav[40:], 3200)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := normalizeVoiceAudio(ctx, wav)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 44 || !bytes.HasPrefix(got, []byte("RIFF")) || string(got[8:12]) != "WAVE" || !bytes.Contains(got, []byte("data")) {
		t.Fatal("normalized output is not WAV audio")
	}

	path := filepath.Join(t.TempDir(), "voice.wav")
	if err := os.WriteFile(path, wav, 0600); err != nil {
		t.Fatal(err)
	}
	playlist := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXTINF:1,\nfile://" + path + "\n#EXT-X-ENDLIST\n"
	if _, err := normalizeVoiceAudio(ctx, []byte(playlist)); err == nil {
		t.Fatal("accepted a playlist referencing an external file")
	}

	cancelled, stop := context.WithCancel(context.Background())
	stop()
	if _, err := normalizeVoiceAudio(cancelled, wav); err == nil {
		t.Fatal("normalization ignored cancellation")
	}
}
