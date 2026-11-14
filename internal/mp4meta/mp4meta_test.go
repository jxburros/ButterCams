package mp4meta

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFragmentHeaderRoundTrip(t *testing.T) {
	video := FragTrack{ID: VideoTrackID, BaseTime: 90000, Samples: []FragSample{
		{Duration: 6000, Size: 1000}, // keyframe
		{Duration: 6000, Size: 200, NonSync: true, PTSOffset: 12000},
		{Duration: 3000, Size: 300, NonSync: true, PTSOffset: -3000},
	}}
	audio := FragTrack{ID: AudioTrackID, BaseTime: 48000, Samples: []FragSample{{Duration: 1024, Size: 90}, {Duration: 1024, Size: 95}}}
	hdr := AppendFragmentHeader(nil, 7, []FragTrack{video, audio})

	moofSize := int(binary.BigEndian.Uint32(hdr))
	if string(hdr[4:8]) != "moof" || len(hdr) != moofSize+8 || string(hdr[moofSize+4:]) != "mdat" {
		t.Fatalf("layout wrong: %d bytes, moof %d", len(hdr), moofSize)
	}
	if got := binary.BigEndian.Uint32(hdr[moofSize:]); got != 8+1500+185 {
		t.Fatalf("mdat size %d", got)
	}
	fi := parseMoof(hdr[8:moofSize])
	if !fi.hasVideo || fi.base != 90000 || fi.duration != 15000 || !fi.firstSync {
		t.Fatalf("video part parsed as %+v", fi)
	}
	// The data offsets must point right behind the mdat header, video first.
	var offsets []uint32
	children(hdr[8:moofSize], func(typ string, traf []byte) {
		children(traf, func(t string, p []byte) {
			if t == "trun" {
				offsets = append(offsets, binary.BigEndian.Uint32(p[8:]))
			}
		})
	})
	if len(offsets) != 2 || int(offsets[0]) != moofSize+8 || int(offsets[1]) != moofSize+8+1500 {
		t.Fatalf("data offsets %v with a %d byte moof", offsets, moofSize)
	}
	// A fragment that does not start with a keyframe is recognised as such.
	late := AppendFragmentHeader(nil, 8, []FragTrack{{ID: VideoTrackID, Samples: []FragSample{{Duration: 1, Size: 1, NonSync: true}}}})
	if fi := parseMoof(late[8:binary.BigEndian.Uint32(late)]); fi.firstSync {
		t.Fatal("a fragment starting with a non-keyframe was reported as starting with a keyframe")
	}
	// Appending to a buffer that already has content keeps that content.
	both := AppendFragmentHeader([]byte("xyz"), 7, []FragTrack{video, audio})
	if !bytes.Equal(both[3:], hdr) || string(both[:3]) != "xyz" {
		t.Fatal("append does not preserve the existing buffer")
	}
}

func TestTrailerAndUdtaAreReadBack(t *testing.T) {
	start := time.Date(2026, 11, 14, 9, 0, 0, 212_000_000, time.FixedZone("", -6*3600))
	ftyp := box("ftyp", []byte("mp42\x00\x00\x00\x01isom"))
	moov, err := AppendToMoov(append(append([]byte{}, ftyp...), box("moov", box("mvhd", make([]byte, 100)))...), Udta("front-door", start, true))
	if err != nil {
		t.Fatal(err)
	}
	frag := AppendFragmentHeader(nil, 1, []FragTrack{{ID: VideoTrackID, BaseTime: 0, Samples: []FragSample{{Duration: 90000, Size: 4}}}})
	body := append(append(append([]byte{}, moov...), frag...), 1, 2, 3, 4)
	file := append(append([]byte{}, body...), Trailer("hour_rollover", 90000, []Fragment{{Time: 0, MoofOffset: uint64(len(moov))}})...)

	path := filepath.Join(t.TempDir(), "a.mp4")
	os.WriteFile(path, file, 0o644)
	info, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.CameraID != "front-door" || !info.Start.Equal(start) || !info.Gap || info.CloseReason != "hour_rollover" ||
		info.Dur90k != 90000 || info.Size != int64(len(file)) || !info.End().Equal(start.Add(time.Second)) {
		t.Fatalf("fast path: %+v", info)
	}
	// The last box must be mfra, ending in an mfro that gives its size.
	tail := file[len(file)-16:]
	if string(tail[4:8]) != "mfro" || string(file[len(file)-int(binary.BigEndian.Uint32(tail[12:]))+4:][:4]) != "mfra" {
		t.Fatal("mfra/mfro are not the last boxes")
	}

	// Without the trailer (a crash), the slow path gives the same answers
	// except for the close reason, and Repair restores a well-formed file.
	cut := append(append([]byte{}, body...), frag[:20]...) // a second fragment was being written
	os.WriteFile(path, cut, 0o644)
	info, err = Read(path)
	if err != nil || info.CloseReason != "" || info.Dur90k != 90000 || info.CameraID != "front-door" {
		t.Fatalf("slow path: %+v %v", info, err)
	}
	info, sum, err := Repair(path, "crash_recovered")
	if err != nil || info.CloseReason != "crash_recovered" || info.Dur90k != 90000 {
		t.Fatalf("repair: %+v %v", info, err)
	}
	fixed, _ := os.ReadFile(path)
	if !bytes.Equal(fixed[:len(body)], body) || int64(len(fixed)) != info.Size {
		t.Fatal("repair changed the recorded data or reported the wrong size")
	}
	if got, _, _ := HashFile(path); got != sum {
		t.Fatal("repair returned the wrong hash")
	}
	again, _ := Read(path)
	if again.CloseReason != "crash_recovered" || again.Dur90k != 90000 {
		t.Fatalf("after repair: %+v", again)
	}

	// A file with nothing recorded in it cannot be repaired.
	os.WriteFile(path, moov, 0o644)
	if _, _, err := Repair(path, "crash_recovered"); err != ErrNoFragments {
		t.Fatalf("empty file: %v", err)
	}
}

func TestTicksToDuration(t *testing.T) {
	if got := TicksToDuration(90000 * 3600); got != time.Hour {
		t.Fatalf("one hour of ticks = %v", got)
	}
	if got := TicksToDuration(45000); got != 500*time.Millisecond {
		t.Fatalf("half a second of ticks = %v", got)
	}
	// 30 days of ticks must not overflow.
	if got := TicksToDuration(90000 * 86400 * 30); got != 30*24*time.Hour {
		t.Fatalf("thirty days of ticks = %v", got)
	}
}
