package mp4meta

import (
	"bytes"
	"encoding/binary"
	"os"
	"strings"
	"testing"
)

// The codec string of a recorded H.265 file comes from the SPS inside its
// hvcC box, not from the box's header copy, which some writers fill with
// the SPS's emulation-prevention bytes left in.
func TestHEVCCodecStringFromTheSPS(t *testing.T) {
	data, err := os.ReadFile("../../test/simulator/samples/h265_1s.h265")
	if err != nil {
		t.Skip(err)
	}
	var sps []byte
	for _, n := range bytes.Split(data, []byte{0, 0, 1}) {
		n = bytes.TrimRight(n, "\x00")
		if len(n) > 2 && (n[0]>>1)&0x3F == 33 {
			sps = n
			break
		}
	}
	if sps == nil {
		t.Fatal("no SPS in the sample")
	}
	want := HEVCCodecString("hvc1", sps)
	if !strings.HasPrefix(want, "hvc1.") || strings.Count(want, ".") < 3 {
		t.Fatalf("codec string from the SPS: %q", want)
	}

	box := make([]byte, 23)
	box[0] = 1
	copy(box[1:13], []byte{0x01, 0x60, 0, 0, 0, 0x03, 0x00, 0x90, 0x00, 0x00, 0x03, 0x3c}) // the damaged copy
	box[22] = 1
	box = append(box, 33|0x80)
	box = binary.BigEndian.AppendUint16(box, 1)
	box = binary.BigEndian.AppendUint16(box, uint16(len(sps)))
	box = append(box, sps...)
	if got := hevcCodecString("hvc1", box); got != want {
		t.Fatalf("from the box: %q, want %q", got, want)
	}
}
