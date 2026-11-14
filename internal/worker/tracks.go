package worker

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtpfragmented"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph265"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtplpcm"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtpmpeg4audio"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtpsimpleaudio"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h265"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/opus"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
	"github.com/pion/rtp"

	"github.com/jxburros/buttercams/internal/audio"
	"github.com/jxburros/buttercams/internal/mp4meta"
)

// processor turns the RTP packets of one session into samples. It never
// decodes video: it only reassembles access units and reads NAL unit headers
// and parameter sets (spec rule 2).
type processor struct {
	sess *session

	codec  string
	dec264 *rtph264.Decoder
	dec265 *rtph265.Decoder
	dts264 *h264.DTSExtractor
	dts265 *h265.DTSExtractor

	params      videoParams
	paramsDirty bool // parameter sets changed since the caller last looked
	started     bool // a keyframe has been seen
	dtsFailed   bool
	lastDTS     int64
	width       int
	height      int

	audio       audioHandler
	audioParams *audioParams
	audioErr    string // set once when audio processing failed

	out []sample
}

func newProcessor(s *session) (*processor, error) {
	p := &processor{sess: s, codec: s.videoCodec, params: videoParams{codec: s.videoCodec}}
	var err error
	switch f := s.videoFormat.(type) {
	case *format.H264:
		if p.dec264, err = f.CreateDecoder(); err != nil {
			return nil, err
		}
		p.dts264 = &h264.DTSExtractor{}
		p.dts264.Initialize()
		sps, pps := f.SafeParams()
		p.setParam(&p.params.sps, sps)
		p.setParam(&p.params.pps, pps)
	case *format.H265:
		if p.dec265, err = f.CreateDecoder(); err != nil {
			return nil, err
		}
		p.dts265 = &h265.DTSExtractor{}
		p.dts265.Initialize()
		vps, sps, pps := f.SafeParams()
		p.setParam(&p.params.vps, vps)
		p.setParam(&p.params.sps, sps)
		p.setParam(&p.params.pps, pps)
	default:
		return nil, fmt.Errorf("unsupported video format %T", f)
	}
	p.paramsDirty = false

	if s.audio != nil {
		h, err := newAudioHandler(s.audio)
		if err != nil {
			p.audioErr = "audio not recorded: " + err.Error()
		} else {
			p.audio = h
			ap := h.params()
			p.audioParams = &ap
		}
	}
	return p, nil
}

func (p *processor) setParam(dst *[]byte, v []byte) {
	if len(v) == 0 || bytes.Equal(*dst, v) {
		return
	}
	*dst = append([]byte(nil), v...)
	p.paramsDirty = true
	if dst == &p.params.sps {
		p.readSPS()
	}
}

func (p *processor) readSPS() {
	defer func() { recover() }() // a malformed SPS must not take the camera down
	if p.codec == "h264" {
		var sps h264.SPS
		if sps.Unmarshal(p.params.sps) == nil {
			p.width, p.height = sps.Width(), sps.Height()
		}
		return
	}
	var sps h265.SPS
	if sps.Unmarshal(p.params.sps) == nil {
		p.width, p.height = sps.Width(), sps.Height()
	}
}

// takeParamsDirty reports whether the parameter sets changed since the last
// call.
func (p *processor) takeParamsDirty() bool {
	d := p.paramsDirty
	p.paramsDirty = false
	return d
}

// reset forgets partial access units after packets were lost.
func (p *processor) reset() {
	p.started = false
	switch f := p.sess.videoFormat.(type) {
	case *format.H264:
		if d, err := f.CreateDecoder(); err == nil {
			p.dec264 = d
		}
		p.dts264 = &h264.DTSExtractor{}
		p.dts264.Initialize()
	case *format.H265:
		if d, err := f.CreateDecoder(); err == nil {
			p.dec265 = d
		}
		p.dts265 = &h265.DTSExtractor{}
		p.dts265.Initialize()
	}
}

// push processes one packet and returns the samples it completed. The
// returned slice is reused by the next call.
func (p *processor) push(pkt *Packet) []sample {
	p.out = p.out[:0]
	if !pkt.PTSValid {
		return nil
	}
	if pkt.Track == TrackVideo {
		p.pushVideo(pkt)
	} else {
		p.pushAudio(pkt)
	}
	return p.out
}

func (p *processor) pushVideo(pkt *Packet) {
	var au [][]byte
	var err error
	if p.codec == "h264" {
		au, err = p.dec264.Decode(pkt.RTP)
		if err != nil {
			return // more packets needed, or a damaged unit: skip it
		}
	} else {
		au, err = p.dec265.Decode(pkt.RTP)
		if err != nil {
			return
		}
	}
	if len(au) == 0 {
		return
	}

	// Parameter sets sent in-band replace the ones from the SDP.
	hasParams := false
	for _, nalu := range au {
		if len(nalu) == 0 {
			continue
		}
		if p.codec == "h264" {
			switch h264.NALUType(nalu[0] & 0x1F) {
			case h264.NALUTypeSPS:
				p.setParam(&p.params.sps, nalu)
				hasParams = true
			case h264.NALUTypePPS:
				p.setParam(&p.params.pps, nalu)
			}
		} else {
			switch h265.NALUType((nalu[0] >> 1) & 0x3F) {
			case h265.NALUType_VPS_NUT:
				p.setParam(&p.params.vps, nalu)
			case h265.NALUType_SPS_NUT:
				p.setParam(&p.params.sps, nalu)
				hasParams = true
			case h265.NALUType_PPS_NUT:
				p.setParam(&p.params.pps, nalu)
			}
		}
	}

	key := false
	if p.codec == "h264" {
		key = h264.IsRandomAccess(au)
	} else {
		key = h265.IsRandomAccess(au)
	}
	if !p.started {
		if !key || !p.params.complete() {
			return
		}
		p.started = true
	}

	dts := p.extractDTS(au, pkt.PTS, key, hasParams)
	if p.lastDTS != 0 && dts <= p.lastDTS && dts > p.lastDTS-10*mp4meta.VideoTimescale {
		dts = p.lastDTS + 1 // keep decode times strictly increasing
	}
	p.lastDTS = dts
	pts := pkt.PTS
	if pts < dts {
		pts = dts
	}

	p.out = append(p.out, sample{video: true, pts: pts, dts: dts, key: key, nalus: au, recv: pkt.Recv})
}

// extractDTS derives the decode time from the picture order. Streams without
// B-frames (nearly every camera) have DTS equal to PTS.
func (p *processor) extractDTS(au [][]byte, pts int64, key, hasParams bool) (dts int64) {
	if p.dtsFailed {
		return pts
	}
	defer func() {
		if r := recover(); r != nil {
			p.dtsFailed = true
			dts = pts
		}
	}()
	// The extractor needs to see the parameter sets; cameras that only send
	// them in the SDP get them prepended for this calculation only.
	in := au
	if key && !hasParams {
		in = make([][]byte, 0, len(au)+3)
		if p.codec == "h265" {
			in = append(in, p.params.vps)
		}
		in = append(in, p.params.sps, p.params.pps)
		in = append(in, au...)
	}
	var err error
	if p.codec == "h264" {
		dts, err = p.dts264.Extract(in, pts)
	} else {
		dts, err = p.dts265.Extract(in, pts)
	}
	if err != nil {
		p.dtsFailed = true
		return pts
	}
	return dts
}

func (p *processor) pushAudio(pkt *Packet) {
	if p.audio == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			p.audio = nil
			p.audioErr = fmt.Sprintf("audio not recorded: converter failed (%v)", r)
		}
	}()
	samples, err := p.audio.push(pkt.RTP, pkt.PTS)
	if err != nil {
		p.audio = nil
		p.audioErr = "audio not recorded: " + err.Error()
		return
	}
	for i := range samples {
		samples[i].recv = pkt.Recv
	}
	p.out = append(p.out, samples...)
}

// ------------------------------------------------------------------ audio

// audioHandler turns the RTP packets of one audio track into samples.
type audioHandler interface {
	push(pkt *rtp.Packet, pts int64) ([]sample, error)
	params() audioParams
}

func newAudioHandler(t *audioTrack) (audioHandler, error) {
	switch f := t.format.(type) {
	case *format.MPEG4Audio:
		dec, err := f.CreateDecoder()
		if err != nil {
			return nil, err
		}
		return &aacHandler{dec: dec, cfg: *f.Config, rate: f.ClockRate()}, nil
	case *format.MPEG4AudioLATM:
		dec, err := f.CreateDecoder()
		if err != nil {
			return nil, err
		}
		cfg := f.StreamMuxConfig.Programs[0].Layers[0].AudioSpecificConfig
		if cfg == nil {
			return nil, errors.New("AAC without a configuration")
		}
		return &latmHandler{dec: dec, mux: f.StreamMuxConfig, cfg: *cfg, rate: f.ClockRate()}, nil
	case *format.Opus:
		dec, err := f.CreateDecoder()
		if err != nil {
			return nil, err
		}
		ch := f.ChannelCount
		if ch == 0 {
			ch = 2
		}
		return &opusHandler{dec: dec, channels: ch}, nil
	case *format.G711:
		dec, err := f.CreateDecoder()
		if err != nil {
			return nil, err
		}
		tr, err := audio.NewTranscoder(f.SampleRate)
		if err != nil {
			return nil, err
		}
		return &pcmHandler{g711: dec, mulaw: f.MULaw, tr: tr}, nil
	case *format.G726:
		rate := f.BitRate
		if rate < 1000 {
			rate *= 1000
		}
		dec, err := audio.NewG726Decoder(rate, f.BigEndian)
		if err != nil {
			return nil, err
		}
		tr, err := audio.NewTranscoder(8000)
		if err != nil {
			return nil, err
		}
		return &pcmHandler{g726: dec, tr: tr}, nil
	}
	return nil, fmt.Errorf("unsupported audio format %s", t.format.Codec())
}

// aacHandler copies AAC access units as received.
type aacHandler struct {
	dec  *rtpmpeg4audio.Decoder
	cfg  mpeg4audio.AudioSpecificConfig
	rate int
	out  []sample
}

func (h *aacHandler) params() audioParams {
	return audioParams{name: "aac", codec: &codecs.MPEG4Audio{Config: h.cfg}, timescale: uint32(h.rate)}
}

func (h *aacHandler) push(pkt *rtp.Packet, pts int64) ([]sample, error) {
	aus, err := h.dec.Decode(pkt)
	if err != nil {
		return nil, nil // fragment of a larger unit, or a damaged packet
	}
	h.out = h.out[:0]
	for i, au := range aus {
		h.out = append(h.out, sample{pts: pts + int64(i)*mpeg4audio.SamplesPerAccessUnit, dts: pts + int64(i)*mpeg4audio.SamplesPerAccessUnit,
			dur: mpeg4audio.SamplesPerAccessUnit, key: true, data: au})
	}
	return h.out, nil
}

// latmHandler extracts AAC access units from the LATM payload format.
type latmHandler struct {
	dec  *rtpfragmented.Decoder
	mux  *mpeg4audio.StreamMuxConfig
	cfg  mpeg4audio.AudioSpecificConfig
	rate int
	out  []sample
}

func (h *latmHandler) params() audioParams {
	return audioParams{name: "aac", codec: &codecs.MPEG4Audio{Config: h.cfg}, timescale: uint32(h.rate)}
}

func (h *latmHandler) push(pkt *rtp.Packet, pts int64) ([]sample, error) {
	buf, err := h.dec.Decode(pkt)
	if err != nil {
		return nil, nil
	}
	ame := mpeg4audio.AudioMuxElement{MuxConfigPresent: false, StreamMuxConfig: h.mux}
	if err := ame.Unmarshal(buf); err != nil {
		return nil, nil
	}
	h.out = h.out[:0]
	n := int64(0)
	for _, subframe := range ame.Payloads {
		if len(subframe) == 0 || len(subframe[0]) == 0 {
			continue
		}
		t := pts + n*mpeg4audio.SamplesPerAccessUnit
		h.out = append(h.out, sample{pts: t, dts: t, dur: mpeg4audio.SamplesPerAccessUnit, key: true, data: subframe[0][0]})
		n++
	}
	return h.out, nil
}

// opusHandler copies Opus packets as received.
type opusHandler struct {
	dec      *rtpsimpleaudio.Decoder
	channels int
	out      []sample
}

func (h *opusHandler) params() audioParams {
	return audioParams{name: "opus", codec: &codecs.Opus{ChannelCount: h.channels}, timescale: 48000}
}

func (h *opusHandler) push(pkt *rtp.Packet, pts int64) ([]sample, error) {
	data, err := h.dec.Decode(pkt)
	if err != nil || len(data) == 0 {
		return nil, nil
	}
	dur := opus.PacketDuration2(data)
	if dur <= 0 {
		return nil, nil
	}
	h.out = append(h.out[:0], sample{pts: pts, dts: pts, dur: uint32(dur), key: true, data: data})
	return h.out, nil
}

// pcmHandler decodes G.711 or G.726 and re-encodes it as AAC-LC, the only
// transcoding ButterCams does.
type pcmHandler struct {
	g711  *rtplpcm.Decoder
	mulaw bool
	g726  *audio.G726Decoder
	tr    *audio.Transcoder
	pcm   []int16
	out   []sample
}

func (h *pcmHandler) params() audioParams {
	return audioParams{
		name: "aac",
		codec: &codecs.MPEG4Audio{Config: mpeg4audio.AudioSpecificConfig{
			Type: mpeg4audio.ObjectTypeAACLC, SampleRate: audio.OutRate, ChannelConfig: 1, ChannelCount: 1,
		}},
		timescale: audio.OutRate,
	}
}

func (h *pcmHandler) push(pkt *rtp.Packet, pts int64) ([]sample, error) {
	h.pcm = h.pcm[:0]
	if h.g711 != nil {
		raw, err := h.g711.Decode(pkt)
		if err != nil {
			return nil, nil
		}
		if h.mulaw {
			h.pcm = audio.DecodeMulaw(h.pcm, raw)
		} else {
			h.pcm = audio.DecodeAlaw(h.pcm, raw)
		}
	} else {
		h.pcm = h.g726.Decode(h.pcm, pkt.Payload)
	}
	frames, err := h.tr.Write(h.pcm, pts)
	if err != nil {
		return nil, err
	}
	h.out = h.out[:0]
	for _, f := range frames {
		h.out = append(h.out, sample{pts: f.PTS, dts: f.PTS, dur: audio.FrameSize, key: true, data: f.Data})
	}
	return h.out, nil
}
