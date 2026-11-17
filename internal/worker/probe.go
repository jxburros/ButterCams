package worker

import (
	"context"
	"time"

	"github.com/jxburros/buttercams/internal/config"
	"github.com/jxburros/buttercams/internal/mp4meta"
)

// KeyframeWarning is shown when a camera's keyframes are too far apart.
const KeyframeWarning = "Set the camera's I-frame interval to 1–2 seconds for clean hourly rollovers."

// ProbeResult is what "Test connection" reports (spec 11.2).
type ProbeResult struct {
	OK                   bool     `json:"ok"`
	Error                string   `json:"error"`
	VideoCodec           string   `json:"videoCodec"`
	Width                int      `json:"width"`
	Height               int      `json:"height"`
	FPS                  float64  `json:"fps"`
	KeyframeIntervalMs   int      `json:"keyframeIntervalMs"`
	AudioCodec           string   `json:"audioCodec"`
	AudioNote            string   `json:"audioNote"`
	SecondSessionAllowed *bool    `json:"secondSessionAllowed"`
	Warnings             []string `json:"warnings"`
}

// probeWatch is how long the probe watches the stream to measure it.
const probeWatch = 8 * time.Second

// Probe connects to a camera once, without recording anything, and reports
// what it sends. It also tries a second session, because cameras that refuse
// one cannot be refreshed without a gap.
func Probe(ctx context.Context, cam config.Camera, password string) ProbeResult {
	res := ProbeResult{Warnings: []string{}}
	pkts := make(chan *Packet, 2048)
	opt := dialOptions{id: 1, cam: cam, password: password, onPacket: func(p *Packet) {
		select {
		case pkts <- p:
		default:
		}
	}}
	sess, err := dialSession(ctx, opt)
	if err != nil {
		res.Error = classifyDialError(err).msg
		return res
	}
	defer sess.close()
	proc, perr := newProcessor(sess)
	if perr != nil {
		res.Error = perr.Error()
		return res
	}

	res.VideoCodec = sess.videoCodec
	switch {
	case proc.audioErr != "":
		res.AudioNote = proc.audioErr
	case sess.audio != nil:
		res.AudioCodec = sess.audio.source
		if sess.audio.source != "aac" && sess.audio.source != "opus" {
			res.AudioNote = "will be converted to AAC"
		}
	case sess.audioNote != "":
		res.AudioNote = sess.audioNote
	case !cam.RecordAudio:
		res.AudioNote = "audio recording is turned off for this camera"
	}

	// Watch the stream: frame rate and distance between keyframes.
	var frames int
	var firstDTS, lastDTS, lastKey int64
	var intervals []int64
	deadline := time.NewTimer(probeWatch)
	defer deadline.Stop()
watch:
	for {
		select {
		case p := <-pkts:
			for _, u := range proc.push(p) {
				if !u.video {
					continue
				}
				if frames == 0 {
					firstDTS = u.dts
				}
				frames++
				lastDTS = u.dts
				if u.key {
					if lastKey != 0 && u.dts > lastKey {
						intervals = append(intervals, u.dts-lastKey)
					}
					lastKey = u.dts
				}
			}
			if len(intervals) >= 2 && frames >= 30 {
				break watch
			}
		case <-sess.done:
			res.Error = "the camera closed the connection during the test"
			return res
		case <-deadline.C:
			break watch
		case <-ctx.Done():
			res.Error = "the test was cancelled"
			return res
		}
	}
	if frames == 0 {
		res.Error = "connected, but the camera sent no video"
		return res
	}
	res.OK = true
	res.Width, res.Height = proc.width, proc.height
	if frames > 1 && lastDTS > firstDTS {
		fps := float64(frames-1) * mp4meta.VideoTimescale / float64(lastDTS-firstDTS)
		res.FPS = float64(int(fps*10+0.5)) / 10
	}
	if len(intervals) > 0 {
		var max int64
		for _, v := range intervals {
			if v > max {
				max = v
			}
		}
		res.KeyframeIntervalMs = int(max * 1000 / mp4meta.VideoTimescale)
		if res.KeyframeIntervalMs > 2000 {
			res.Warnings = append(res.Warnings, KeyframeWarning)
		}
	} else {
		res.Warnings = append(res.Warnings,
			"No second keyframe arrived within 8 seconds. "+KeyframeWarning)
	}

	// Second session while the first is still playing.
	second, err := dialSession(ctx, dialOptions{id: 2, cam: cam, password: password})
	switch {
	case err == nil:
		second.close()
		yes := true
		res.SecondSessionAllowed = &yes
	case classifyDialError(err).kind == dialRefused:
		no := false
		res.SecondSessionAllowed = &no
		res.Warnings = append(res.Warnings,
			"The camera allows only one connection, so the hourly feed refresh leaves a short gap.")
	}
	return res
}
