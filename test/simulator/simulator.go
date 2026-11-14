// Package simulator is the camera simulator used by the integration tests,
// the chaos scripts and manual load tests (spec 14.1). It is a small RTSP
// server that serves any number of looping streams from embedded sample
// files, with switches for the situations the recorder must survive: H.264,
// H.265, an unsupported video codec, AAC, G.711, G.726, unsupported audio, no
// audio, 1 s or 4 s keyframe interval, single-session cameras, stalls and
// authentication failures.
package simulator

import (
	"embed"
	"fmt"
	"math"
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/liberrors"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/g711"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	"github.com/pion/rtp"
	aac "github.com/tphakala/go-aac"
)

//go:embed samples/h264_1s.h264 samples/h264_4s.h264 samples/h265_1s.h265 samples/g726_32.bin
var samples embed.FS

// FPS is the frame rate of the embedded sample streams.
const FPS = 15

// Stream configures one simulated camera.
type Stream struct {
	// Path is the RTSP path, for example "/cam1".
	Path string
	// Video is "h264" (default), "h265" or "mjpeg" (a codec ButterCams does
	// not record).
	Video string
	// KeyframeInterval is 1 s (default) or 4 s. H.264 only.
	KeyframeInterval time.Duration
	// Audio is "none" (default), "aac", "pcmu", "pcma", "g726" or "speex"
	// (a codec ButterCams does not record).
	Audio string
	// SingleSession makes the camera refuse a second concurrent session with
	// RTSP 453, like many cheap cameras do.
	SingleSession bool
	// Username and Password, when set, are required from clients.
	Username string
	Password string
	// StallEvery and StallFor make the stream stop sending for StallFor at
	// random intervals averaging StallEvery.
	StallEvery time.Duration
	StallFor   time.Duration
	// PadBytes adds filler data to every frame to imitate a higher bitrate
	// (33000 ≈ 4 Mbit/s at 15 fps).
	PadBytes int
}

type stream struct {
	cfg    Stream
	st     *gortsplib.ServerStream
	stop   chan struct{}
	failed bool // refuse new sessions with RTSP 500

	mu         sync.Mutex
	stallUntil time.Time
	sessions   map[*gortsplib.ServerSession]struct{}
}

// Server is a running simulator.
type Server struct {
	srv *gortsplib.Server
	ln  string

	mu      sync.RWMutex
	streams map[string]*stream
	wg      sync.WaitGroup
}

// Start launches a simulator on addr ("127.0.0.1:0" picks a free port).
func Start(addr string, streams []Stream) (*Server, error) {
	if strings.HasSuffix(addr, ":0") {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, err
		}
		addr = l.Addr().String()
		l.Close()
	}
	s := &Server{ln: addr, streams: map[string]*stream{}}
	s.srv = &gortsplib.Server{Handler: s, RTSPAddress: addr}
	if err := s.srv.Start(); err != nil {
		return nil, err
	}
	for _, cfg := range streams {
		if err := s.Add(cfg); err != nil {
			s.Close()
			return nil, err
		}
	}
	return s, nil
}

// Addr returns the host:port the simulator listens on.
func (s *Server) Addr() string { return s.ln }

// URL returns the rtsp:// URL of a stream.
func (s *Server) URL(path string) string { return "rtsp://" + s.ln + path }

// Close stops the simulator.
func (s *Server) Close() {
	s.mu.Lock()
	for _, st := range s.streams {
		close(st.stop)
	}
	s.mu.Unlock()
	s.wg.Wait()
	s.mu.Lock()
	for _, st := range s.streams {
		if st.st != nil {
			st.st.Close()
		}
	}
	s.streams = map[string]*stream{}
	s.mu.Unlock()
	s.srv.Close()
}

func (s *Server) get(path string) *stream {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	path = "/" + strings.Trim(path, "/")
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.streams[path]
}

// Stall makes a stream stop sending for d.
func (s *Server) Stall(path string, d time.Duration) {
	if st := s.get(path); st != nil {
		st.mu.Lock()
		st.stallUntil = time.Now().Add(d)
		st.mu.Unlock()
	}
}

// Kick closes every session of a stream, like a camera rebooting.
func (s *Server) Kick(path string) {
	st := s.get(path)
	if st == nil {
		return
	}
	st.mu.Lock()
	var list []*gortsplib.ServerSession
	for ss := range st.sessions {
		list = append(list, ss)
	}
	st.mu.Unlock()
	for _, ss := range list {
		ss.Close()
	}
}

// FailNewSessions makes a stream answer new connections with RTSP 500 while
// existing sessions keep playing.
func (s *Server) FailNewSessions(path string, on bool) {
	if st := s.get(path); st != nil {
		st.mu.Lock()
		st.failed = on
		st.mu.Unlock()
	}
}

// Sessions returns the number of sessions currently set up on a stream.
func (s *Server) Sessions(path string) int {
	st := s.get(path)
	if st == nil {
		return 0
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.sessions)
}

// splitAnnexB splits an Annex-B byte stream (NAL units separated by 00 00 01
// or 00 00 00 01 start codes) into NAL units.
func splitAnnexB(data []byte) [][]byte {
	var out [][]byte
	start := -1
	i := 0
	for i+2 < len(data) {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			if start >= 0 {
				end := i
				for end > start && data[end-1] == 0 {
					end-- // trailing zero bytes belong to the next start code
				}
				if end > start {
					out = append(out, data[start:end])
				}
			}
			start = i + 3
			i += 3
			continue
		}
		i++
	}
	if start >= 0 && start < len(data) {
		out = append(out, data[start:])
	}
	return out
}

type videoSource struct {
	format format.Format
	aus    [][][]byte
	encode func(au [][]byte) ([]*rtp.Packet, error)
	filler func(n int) []byte
}

func loadVideo(cfg Stream) (*videoSource, error) {
	switch cfg.Video {
	case "", "h264":
		name := "samples/h264_1s.h264"
		if cfg.KeyframeInterval >= 3*time.Second {
			name = "samples/h264_4s.h264"
		}
		data, err := samples.ReadFile(name)
		if err != nil {
			return nil, err
		}
		nalus := splitAnnexB(data)
		var sps, pps []byte
		var aus [][][]byte
		var cur [][]byte
		for _, n := range nalus {
			switch h264.NALUType(n[0] & 0x1F) {
			case h264.NALUTypeAccessUnitDelimiter:
				if len(cur) > 0 {
					aus = append(aus, cur)
				}
				cur = nil
				continue
			case h264.NALUTypeSPS:
				sps = n
			case h264.NALUTypePPS:
				pps = n
			case h264.NALUTypeSEI:
				continue
			}
			cur = append(cur, n)
		}
		if len(cur) > 0 {
			aus = append(aus, cur)
		}
		f := &format.H264{PayloadTyp: 96, PacketizationMode: 1, SPS: sps, PPS: pps}
		enc, err := f.CreateEncoder()
		if err != nil {
			return nil, err
		}
		return &videoSource{format: f, aus: aus, encode: enc.Encode, filler: func(n int) []byte {
			b := make([]byte, n+2)
			b[0] = byte(h264.NALUTypeFillerData)
			for i := 1; i <= n; i++ {
				b[i] = 0xFF
			}
			b[n+1] = 0x80
			return b
		}}, nil

	case "h265":
		data, err := samples.ReadFile("samples/h265_1s.h265")
		if err != nil {
			return nil, err
		}
		nalus := splitAnnexB(data)
		var vps, sps, pps []byte
		var aus [][][]byte
		var cur [][]byte
		for _, n := range nalus {
			switch (n[0] >> 1) & 0x3F {
			case 35: // access unit delimiter
				if len(cur) > 0 {
					aus = append(aus, cur)
				}
				cur = nil
				continue
			case 32:
				vps = n
			case 33:
				sps = n
			case 34:
				pps = n
			case 39, 40: // SEI
				continue
			}
			cur = append(cur, n)
		}
		if len(cur) > 0 {
			aus = append(aus, cur)
		}
		f := &format.H265{PayloadTyp: 96, VPS: vps, SPS: sps, PPS: pps}
		enc, err := f.CreateEncoder()
		if err != nil {
			return nil, err
		}
		return &videoSource{format: f, aus: aus, encode: enc.Encode, filler: func(n int) []byte {
			b := make([]byte, n+3)
			b[0], b[1] = 38<<1, 1 // filler data NAL unit
			for i := 2; i < n+2; i++ {
				b[i] = 0xFF
			}
			b[n+2] = 0x80
			return b
		}}, nil

	case "mjpeg":
		return &videoSource{format: &format.MJPEG{}}, nil
	}
	return nil, fmt.Errorf("simulator: unknown video codec %q", cfg.Video)
}

// audioSource produces RTP packets for a span of time.
type audioSource struct {
	format format.Format
	rate   int
	// next returns the packets that start before sample position `until`.
	next func(until int64) []*rtp.Packet
}

func toneSample(i int64, rate int) float64 {
	t := float64(i) / float64(rate)
	return 0.3*math.Sin(2*math.Pi*440*t) + 0.15*math.Sin(2*math.Pi*1000*t)
}

func loadAudio(kind string) (*audioSource, error) {
	var seq uint16
	var pos int64
	mk := func(pt uint8, ts uint32, payload []byte) *rtp.Packet {
		seq++
		return &rtp.Packet{
			Header:  rtp.Header{Version: 2, PayloadType: pt, SequenceNumber: seq, Timestamp: ts, SSRC: 0x5151, Marker: true},
			Payload: payload,
		}
	}
	switch kind {
	case "", "none":
		return nil, nil

	case "pcmu", "pcma":
		mulaw := kind == "pcmu"
		pt := uint8(8)
		if mulaw {
			pt = 0
		}
		f := &format.G711{PayloadTyp: pt, MULaw: mulaw, SampleRate: 8000, ChannelCount: 1}
		const frame = 160 // 20 ms
		return &audioSource{format: f, rate: 8000, next: func(until int64) []*rtp.Packet {
			var out []*rtp.Packet
			for pos < until {
				lpcm := make([]byte, frame*2)
				for i := 0; i < frame; i++ {
					v := int16(toneSample(pos+int64(i), 8000) * 32767)
					lpcm[2*i], lpcm[2*i+1] = byte(v>>8), byte(v)
				}
				var payload []byte
				if mulaw {
					payload, _ = g711.Mulaw(lpcm).Marshal()
				} else {
					payload, _ = g711.Alaw(lpcm).Marshal()
				}
				out = append(out, mk(pt, uint32(pos), payload))
				pos += frame
			}
			return out
		}}, nil

	case "g726":
		data, err := samples.ReadFile("samples/g726_32.bin")
		if err != nil {
			return nil, err
		}
		f := &format.G726{PayloadTyp: 97, BitRate: 32}
		const frame = 160 // samples; 80 bytes at 4 bits per sample
		off := 0
		return &audioSource{format: f, rate: 8000, next: func(until int64) []*rtp.Packet {
			var out []*rtp.Packet
			for pos < until {
				if off+frame/2 > len(data) {
					off = 0
				}
				out = append(out, mk(97, uint32(pos), data[off:off+frame/2]))
				off += frame / 2
				pos += frame
			}
			return out
		}}, nil

	case "aac":
		const rate = 48000
		enc, err := aac.NewEncoder(aac.EncoderConfig{SampleRate: rate, Channels: 1, Bitrate: 64000})
		if err != nil {
			return nil, err
		}
		f := &format.MPEG4Audio{
			PayloadTyp: 97,
			Config: &mpeg4audio.AudioSpecificConfig{
				Type: mpeg4audio.ObjectTypeAACLC, SampleRate: rate, ChannelConfig: 1, ChannelCount: 1,
			},
			SizeLength: 13, IndexLength: 3, IndexDeltaLength: 3,
		}
		renc, err := f.CreateEncoder()
		if err != nil {
			return nil, err
		}
		var encPos int64
		return &audioSource{format: f, rate: rate, next: func(until int64) []*rtp.Packet {
			var out []*rtp.Packet
			for pos < until {
				pcm := make([]float32, aac.FrameSize)
				for i := range pcm {
					pcm[i] = float32(toneSample(encPos+int64(i), rate))
				}
				encPos += aac.FrameSize
				au, err := enc.EncodeFrame(nil, [][]float32{pcm})
				if err != nil || len(au) == 0 {
					continue // the encoder holds back its first frame
				}
				pkts, err := renc.Encode([][]byte{au})
				if err != nil {
					continue
				}
				for _, p := range pkts {
					p.Timestamp = uint32(pos)
					out = append(out, p)
				}
				pos += aac.FrameSize
			}
			return out
		}}, nil

	case "speex":
		return &audioSource{format: &format.Speex{PayloadTyp: 98, SampleRate: 8000}, rate: 8000,
			next: func(int64) []*rtp.Packet { return nil }}, nil
	}
	return nil, fmt.Errorf("simulator: unknown audio codec %q", kind)
}

// Add starts serving another stream.
func (s *Server) Add(cfg Stream) error {
	if !strings.HasPrefix(cfg.Path, "/") {
		cfg.Path = "/" + cfg.Path
	}
	video, err := loadVideo(cfg)
	if err != nil {
		return err
	}
	audio, err := loadAudio(cfg.Audio)
	if err != nil {
		return err
	}
	videoMedia := &description.Media{Type: description.MediaTypeVideo, Formats: []format.Format{video.format}}
	desc := &description.Session{Medias: []*description.Media{videoMedia}}
	var audioMedia *description.Media
	if audio != nil {
		audioMedia = &description.Media{Type: description.MediaTypeAudio, Formats: []format.Format{audio.format}}
		desc.Medias = append(desc.Medias, audioMedia)
	}
	st := &stream{cfg: cfg, stop: make(chan struct{}), sessions: map[*gortsplib.ServerSession]struct{}{}}
	st.st = &gortsplib.ServerStream{Server: s.srv, Desc: desc}
	if err := st.st.Initialize(); err != nil {
		return err
	}
	s.mu.Lock()
	if _, dup := s.streams[cfg.Path]; dup {
		s.mu.Unlock()
		st.st.Close()
		return fmt.Errorf("simulator: duplicate path %s", cfg.Path)
	}
	s.streams[cfg.Path] = st
	s.mu.Unlock()

	if len(video.aus) == 0 {
		return nil // an unsupported codec: nothing to send
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		st.run(video, videoMedia, audio, audioMedia)
	}()
	return nil
}

func (st *stream) run(video *videoSource, videoMedia *description.Media, audio *audioSource, audioMedia *description.Media) {
	interval := time.Second / FPS
	start := time.Now()
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	base := rng.Uint32()
	var filler []byte
	if st.cfg.PadBytes > 0 {
		filler = video.filler(st.cfg.PadBytes)
	}
	nextRandomStall := time.Time{}
	if st.cfg.StallEvery > 0 {
		nextRandomStall = start.Add(time.Duration(rng.Float64() * 2 * float64(st.cfg.StallEvery)))
	}

	for n := int64(0); ; n++ {
		due := start.Add(time.Duration(n) * interval)
		select {
		case <-st.stop:
			return
		case <-time.After(time.Until(due)):
		}
		now := time.Now()
		st.mu.Lock()
		if !nextRandomStall.IsZero() && now.After(nextRandomStall) {
			st.stallUntil = now.Add(st.cfg.StallFor)
			nextRandomStall = now.Add(st.cfg.StallFor + time.Duration(rng.Float64()*2*float64(st.cfg.StallEvery)))
		}
		stalled := now.Before(st.stallUntil)
		st.mu.Unlock()

		var audioPkts []*rtp.Packet
		if audio != nil {
			// Audio is generated even while stalled so its clock keeps running.
			audioPkts = audio.next((n + 1) * int64(audio.rate) / FPS)
		}
		if stalled {
			continue
		}

		au := video.aus[n%int64(len(video.aus))]
		if filler != nil {
			au = append(append(make([][]byte, 0, len(au)+1), au...), filler)
		}
		pkts, err := video.encode(au)
		if err != nil {
			continue
		}
		ts := base + uint32(n*90000/FPS)
		for _, p := range pkts {
			p.Timestamp = ts
			st.st.WritePacketRTP(videoMedia, p) //nolint:errcheck
		}
		for _, p := range audioPkts {
			st.st.WritePacketRTP(audioMedia, p) //nolint:errcheck
		}
	}
}

// ---- gortsplib.ServerHandler

// OnConnOpen implements gortsplib.ServerHandlerOnConnOpen.
func (s *Server) OnConnOpen(*gortsplib.ServerHandlerOnConnOpenCtx) {}

// OnConnClose implements gortsplib.ServerHandlerOnConnClose.
func (s *Server) OnConnClose(*gortsplib.ServerHandlerOnConnCloseCtx) {}

// OnSessionOpen implements gortsplib.ServerHandlerOnSessionOpen.
func (s *Server) OnSessionOpen(*gortsplib.ServerHandlerOnSessionOpenCtx) {}

// OnSessionClose implements gortsplib.ServerHandlerOnSessionClose.
func (s *Server) OnSessionClose(ctx *gortsplib.ServerHandlerOnSessionCloseCtx) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, st := range s.streams {
		st.mu.Lock()
		delete(st.sessions, ctx.Session)
		st.mu.Unlock()
	}
}

func (s *Server) admit(conn *gortsplib.ServerConn, req *base.Request, path string, sess *gortsplib.ServerSession) (*base.Response, *stream, error) {
	st := s.get(path)
	if st == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	if st.cfg.Username != "" && !conn.VerifyCredentials(req, st.cfg.Username, st.cfg.Password) {
		return &base.Response{StatusCode: base.StatusUnauthorized}, nil, liberrors.ErrServerAuth{}
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	_, known := st.sessions[sess]
	if sess == nil || !known {
		if st.failed {
			return &base.Response{StatusCode: base.StatusInternalServerError}, nil, nil
		}
		if st.cfg.SingleSession && len(st.sessions) >= 1 {
			return &base.Response{StatusCode: base.StatusNotEnoughBandwidth}, nil, nil
		}
	}
	if sess != nil {
		st.sessions[sess] = struct{}{}
	}
	return &base.Response{StatusCode: base.StatusOK}, st, nil
}

// OnDescribe implements gortsplib.ServerHandlerOnDescribe.
func (s *Server) OnDescribe(ctx *gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	res, st, err := s.admit(ctx.Conn, ctx.Request, ctx.Path, nil)
	if st == nil {
		return res, nil, err
	}
	return res, st.st, nil
}

// OnSetup implements gortsplib.ServerHandlerOnSetup.
func (s *Server) OnSetup(ctx *gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	res, st, err := s.admit(ctx.Conn, ctx.Request, ctx.Path, ctx.Session)
	if st == nil {
		return res, nil, err
	}
	return res, st.st, nil
}

// OnPlay implements gortsplib.ServerHandlerOnPlay.
func (s *Server) OnPlay(*gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	return &base.Response{StatusCode: base.StatusOK}, nil
}
