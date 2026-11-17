package mp4meta

import "testing"

func TestHEVCCodecString(t *testing.T) {
	// Main profile, main tier, level 3.1, compatibility flags 0x60000000,
	// progressive + frame-only constraint flags (0xB0).
	hvcC := []byte{1, 0x01, 0x60, 0, 0, 0, 0xB0, 0, 0, 0, 0, 0, 93}
	if got := hevcCodecString("hvc1", hvcC); got != "hvc1.1.6.L93.B0" {
		t.Fatalf("got %q", got)
	}
	// Main 10, high tier, level 5.1, no constraint flags, an old hev1 file.
	hvcC = []byte{1, 0x22, 0x20, 0, 0, 0, 0, 0, 0, 0, 0, 0, 153}
	if got := hevcCodecString("hev1", hvcC); got != "hev1.2.4.H153" {
		t.Fatalf("got %q", got)
	}
	if hevcCodecString("hvc1", []byte{1, 2}) != "" {
		t.Fatal("a short hvcC must give no string")
	}
}

func TestESDSObjectType(t *testing.T) {
	// ES_Descr → DecoderConfigDescr (0x40, audio) → DecoderSpecificInfo with
	// an AAC-LC AudioSpecificConfig, lengths in the long form ffmpeg writes.
	esds := []byte{0, 0, 0, 0,
		0x03, 0x80, 0x80, 0x80, 0x22, 0, 2, 0,
		0x04, 0x80, 0x80, 0x80, 0x14, 0x40, 0x15, 0, 0, 0, 0, 1, 0xF4, 0, 0, 1, 0xF4, 0,
		0x05, 0x80, 0x80, 0x80, 0x02, 0x14, 0x08,
		0x06, 0x80, 0x80, 0x80, 0x01, 0x02}
	if got := esdsObjectType(esds); got != 2 {
		t.Fatalf("object type %d, want 2 (AAC-LC)", got)
	}
	if esdsObjectType([]byte{0, 0, 0, 0, 0x03, 0x01}) != 0 {
		t.Fatal("a damaged esds must give 0")
	}
}
