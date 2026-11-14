// Command camsim runs the camera simulator: any number of looping RTSP
// streams that behave like IP cameras, including the awkward ones.
//
//	camsim --count 60 --pad 33000              sixty 4 Mbit/s H.264 cameras
//	camsim --video h265 --audio pcmu           H.265 with G.711 audio
//	camsim --single-session --keyint 4         cameras that refuse a second session
//	camsim --stall-every 5m --stall-for 20s    streams that stall at random
//	camsim --user admin --password secret      cameras that require a login
//
// Streams are served at rtsp://<listen>/cam1 … /camN.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jxburros/buttercams/test/simulator"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8554", "address to listen on")
	count := flag.Int("count", 1, "number of cameras")
	video := flag.String("video", "h264", "video codec: h264, h265 or mjpeg (unsupported by ButterCams)")
	audio := flag.String("audio", "none", "audio codec: none, aac, pcmu, pcma, g726 or speex (unsupported)")
	keyint := flag.Int("keyint", 1, "keyframe interval in seconds: 1 or 4 (H.264 only)")
	single := flag.Bool("single-session", false, "refuse a second concurrent session per camera")
	user := flag.String("user", "", "require this user name")
	password := flag.String("password", "", "require this password")
	stallEvery := flag.Duration("stall-every", 0, "average time between random stalls (0 = never)")
	stallFor := flag.Duration("stall-for", 15*time.Second, "length of a random stall")
	pad := flag.Int("pad", 0, "filler bytes added to every frame (33000 is about 4 Mbit/s)")
	flag.Parse()

	var streams []simulator.Stream
	for i := 1; i <= *count; i++ {
		streams = append(streams, simulator.Stream{
			Path: fmt.Sprintf("/cam%d", i), Video: *video, Audio: *audio,
			KeyframeInterval: time.Duration(*keyint) * time.Second, SingleSession: *single,
			Username: *user, Password: *password, StallEvery: *stallEvery, StallFor: *stallFor, PadBytes: *pad,
		})
	}
	srv, err := simulator.Start(*listen, streams)
	if err != nil {
		fmt.Fprintln(os.Stderr, "camsim:", err)
		os.Exit(1)
	}
	fmt.Printf("camsim: %d camera(s) at rtsp://%s/cam1 … /cam%d\n", *count, srv.Addr(), *count)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	srv.Close()
}
