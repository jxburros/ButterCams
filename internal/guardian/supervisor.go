package guardian

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jxburros/buttercams/internal/ipc"
	"github.com/jxburros/buttercams/internal/winsvc"
)

// Supervision timing (spec 9).
const (
	// RestartDelay is the pause before a crashed child is started again; a
	// worker is back within two seconds.
	RestartDelay = time.Second
	// CrashLoopRestarts within CrashLoopWindow raise a Critical status.
	CrashLoopRestarts = 5
	CrashLoopWindow   = 10 * time.Minute
	// MaxRestartDelay caps the backoff of a child that keeps crashing.
	MaxRestartDelay = 60 * time.Second
	// StopGrace is how long children get to finalize files on shutdown.
	StopGrace = 15 * time.Second
)

// ProcSpec describes a supervised process. Workers and the mover are both
// defined this way, and so are the extension processes of later versions
// (spec 15): executable, arguments, heartbeat limit and restart policy.
type ProcSpec struct {
	Name             string   // "worker:3", "mover"
	Exe              string   // executable to run
	Args             []string // its arguments
	Env              []string // extra environment variables
	HeartbeatTimeout time.Duration
	RestartDelay     time.Duration // base delay; 0 = RestartDelay
	MaxRestartDelay  time.Duration // 0 = MaxRestartDelay
	// BelowNormalPriority starts the process at below-normal priority: it
	// then gives way to the recorders when the CPU is busy (spec 16.2).
	BelowNormalPriority bool

	// OnStart runs each time the process has started; it sends the process
	// everything it needs to resume (its cameras, the copy queue …).
	OnStart func(p *Proc)
	// OnMessage receives every message the process sends except heartbeats.
	OnMessage func(p *Proc, msg any)
	// OnRestart is told why the process is being started again.
	OnRestart func(p *Proc, reason string, crashLoop bool)
}

// Proc is a running (or restarting) supervised process.
type Proc struct {
	spec ProcSpec
	log  *slog.Logger
	logw func(line []byte) // receives the child's stderr lines
	job  *winsvc.Job       // guardian-wide kill-on-close job the child is put in

	mu       sync.Mutex
	cmd      *exec.Cmd
	send     chan any
	started  bool
	stopping bool
	restarts []time.Time
	killWhy  string
	exited   chan struct{}

	lastBeat  atomic.Int64 // UnixNano
	downSince atomic.Int64 // UnixNano; when the process last stopped (or was created)
	memBytes  atomic.Uint64
	startedAt atomic.Int64
	crashLoop atomic.Bool
	running   atomic.Bool
	done      chan struct{}
}

func newProc(spec ProcSpec, log *slog.Logger, logw func([]byte)) *Proc {
	if spec.RestartDelay == 0 {
		spec.RestartDelay = RestartDelay
	}
	if spec.MaxRestartDelay == 0 {
		spec.MaxRestartDelay = MaxRestartDelay
	}
	p := &Proc{spec: spec, log: log.With("child", spec.Name), logw: logw, done: make(chan struct{})}
	p.downSince.Store(time.Now().UnixNano())
	return p
}

// Unresponsive reports whether the process has been down, or silent while
// running, for longer than its heartbeat limit. A process that is merely
// starting or being restarted (about a second) does not count.
func (p *Proc) Unresponsive(now time.Time) bool {
	limit := p.spec.HeartbeatTimeout
	if limit <= 0 {
		limit = 30 * time.Second
	}
	if !p.running.Load() {
		return now.Sub(time.Unix(0, p.downSince.Load())) > limit
	}
	return now.Sub(p.LastHeartbeat()) > limit
}

// Name returns the process name.
func (p *Proc) Name() string { return p.spec.Name }

// Running reports whether the process is currently alive.
func (p *Proc) Running() bool { return p.running.Load() }

// LastHeartbeat returns when the process last proved it was alive.
func (p *Proc) LastHeartbeat() time.Time { return time.Unix(0, p.lastBeat.Load()) }

// MemBytes returns the memory the process last reported.
func (p *Proc) MemBytes() uint64 { return p.memBytes.Load() }

// CrashLoop reports whether the process restarted too often recently.
func (p *Proc) CrashLoop() bool { return p.crashLoop.Load() }

// Send queues a message for the process. It never blocks: if the process is
// down or its queue is full the message is dropped, and OnStart brings the
// process up to date when it is back.
func (p *Proc) Send(msg any) bool {
	p.mu.Lock()
	ch := p.send
	p.mu.Unlock()
	if ch == nil {
		return false
	}
	select {
	case ch <- msg:
		return true
	default:
		p.log.Warn("message queue to child is full; dropping message")
		return false
	}
}

// Start launches the supervision loop.
func (p *Proc) Start() {
	p.mu.Lock()
	p.started = true
	p.mu.Unlock()
	go p.supervise()
}

func (p *Proc) supervise() {
	defer close(p.done)
	reason := ""
	for {
		p.mu.Lock()
		if p.stopping {
			p.mu.Unlock()
			return
		}
		p.mu.Unlock()

		func() {
			// A panic in the supervision code must not end supervision.
			defer func() {
				if r := recover(); r != nil {
					p.log.Error("supervisor panicked", "panic", fmt.Sprint(r))
					reason = "supervisor error"
				}
			}()
			reason = p.runOnce()
		}()

		p.mu.Lock()
		if p.stopping {
			p.mu.Unlock()
			return
		}
		now := time.Now()
		p.restarts = append(p.restarts, now)
		for len(p.restarts) > 0 && now.Sub(p.restarts[0]) > CrashLoopWindow {
			p.restarts = p.restarts[1:]
		}
		n := len(p.restarts)
		p.mu.Unlock()

		delay := p.spec.RestartDelay
		loop := n >= CrashLoopRestarts
		if loop {
			// Keep restarting, but back off up to a minute.
			for i := CrashLoopRestarts; i <= n && delay < p.spec.MaxRestartDelay; i++ {
				delay *= 2
			}
			if delay > p.spec.MaxRestartDelay {
				delay = p.spec.MaxRestartDelay
			}
		}
		p.crashLoop.Store(loop)
		p.log.Warn("child stopped; restarting", "reason", reason, "restartsIn10Min", n, "delay", delay.String())
		if p.spec.OnRestart != nil {
			p.spec.OnRestart(p, reason, loop)
		}
		time.Sleep(delay)
	}
}

// runOnce starts the process and returns why it ended.
func (p *Proc) runOnce() string {
	cmd := exec.Command(p.spec.Exe, p.spec.Args...)
	cmd.Env = append(cmd.Environ(), p.spec.Env...)
	winsvc.PrepareChild(cmd)
	if p.spec.BelowNormalPriority {
		winsvc.BelowNormalPriority(cmd)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "cannot create pipe: " + err.Error()
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "cannot create pipe: " + err.Error()
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "cannot create pipe: " + err.Error()
	}
	if err := cmd.Start(); err != nil {
		return "cannot start: " + err.Error()
	}
	if p.job != nil {
		if err := p.job.Assign(cmd.Process.Pid); err != nil {
			// The child still stops by itself when its stdin closes.
			p.log.Warn("could not put the child in the job object", "error", err.Error())
		}
	}

	sendCh := make(chan any, 4096)
	exited := make(chan struct{})
	p.mu.Lock()
	p.cmd, p.send, p.exited, p.killWhy = cmd, sendCh, exited, ""
	p.mu.Unlock()
	now := time.Now()
	p.lastBeat.Store(now.UnixNano())
	p.startedAt.Store(now.UnixNano())
	p.running.Store(true)
	p.log.Info("child started", "pid", cmd.Process.Pid)

	// Writer: the only goroutine that touches the child's stdin, so a child
	// that stops reading can never block the guardian.
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		defer func() { recover() }()
		w := ipc.NewWriter(stdin)
		for msg := range sendCh {
			if err := w.Send(msg); err != nil {
				return
			}
		}
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // stdout: messages
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				p.log.Error("handling a child message panicked", "panic", fmt.Sprint(r))
			}
		}()
		r := ipc.NewReader(stdout)
		for {
			msg, err := r.Next()
			if errors.Is(err, ipc.ErrMalformed) {
				p.log.Warn("malformed message from child", "error", err.Error())
				continue
			}
			if err != nil {
				return // end of output: the child exited or closed its pipe
			}
			if hb, ok := msg.(*ipc.Heartbeat); ok {
				p.lastBeat.Store(time.Now().UnixNano())
				p.memBytes.Store(hb.MemBytes)
				continue
			}
			p.lastBeat.Store(time.Now().UnixNano()) // any message proves life
			if p.spec.OnMessage != nil {
				p.handle(msg)
			}
		}
	}()
	go func() { // stderr: the child's log lines
		defer wg.Done()
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			if p.logw != nil {
				p.logw(sc.Bytes())
			}
		}
	}()

	if p.spec.OnStart != nil {
		func() {
			defer func() {
				if r := recover(); r != nil {
					p.log.Error("child start handler panicked", "panic", fmt.Sprint(r))
				}
			}()
			p.spec.OnStart(p)
		}()
	}

	waitErr := make(chan error, 1)
	go func() {
		wg.Wait() // drain the pipes before Wait closes them
		waitErr <- cmd.Wait()
	}()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var result string
loop:
	for {
		select {
		case err := <-waitErr:
			p.mu.Lock()
			why := p.killWhy
			p.mu.Unlock()
			switch {
			case why != "":
				result = why
			case err != nil:
				result = "exited: " + err.Error()
			default:
				result = "exited"
			}
			break loop
		case <-ticker.C:
			if p.spec.HeartbeatTimeout > 0 && time.Since(p.LastHeartbeat()) > p.spec.HeartbeatTimeout {
				p.kill(fmt.Sprintf("no heartbeat for %s", p.spec.HeartbeatTimeout))
			}
		}
	}

	p.running.Store(false)
	p.downSince.Store(time.Now().UnixNano())
	close(exited)
	p.mu.Lock()
	p.send = nil
	p.cmd = nil
	p.mu.Unlock()
	close(sendCh)
	stdin.Close()
	<-writerDone
	return result
}

func (p *Proc) handle(msg any) {
	defer func() {
		if r := recover(); r != nil {
			p.log.Error("handling a child message panicked", "panic", fmt.Sprint(r), "message", fmt.Sprintf("%T", msg))
		}
	}()
	p.spec.OnMessage(p, msg)
}

// kill terminates the process immediately; supervision restarts it.
func (p *Proc) kill(why string) {
	p.mu.Lock()
	cmd := p.cmd
	if p.killWhy == "" {
		p.killWhy = why
	}
	p.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		p.log.Warn("killing child", "reason", why)
		cmd.Process.Kill()
	}
}

// Kill terminates a hung process; it is restarted.
func (p *Proc) Kill(why string) { p.kill(why) }

// Restart asks the process to finish its work and exit; supervision then
// starts it again. If it does not exit within the grace period it is killed.
func (p *Proc) Restart(why string, grace time.Duration) {
	p.mu.Lock()
	exited := p.exited
	if p.killWhy == "" {
		p.killWhy = why
	}
	p.mu.Unlock()
	if exited == nil {
		return
	}
	p.Send(&ipc.Shutdown{})
	go func() {
		select {
		case <-exited:
		case <-time.After(grace):
			p.kill(why + " (did not exit in time)")
		}
	}()
}

// Stop ends supervision: the process is asked to shut down, given the grace
// period to finalize its files, then killed.
func (p *Proc) Stop(grace time.Duration) {
	p.mu.Lock()
	p.stopping = true
	exited := p.exited
	running := p.cmd != nil
	started := p.started
	p.mu.Unlock()
	if !started {
		return
	}
	if running {
		p.Send(&ipc.Shutdown{})
		select {
		case <-exited:
		case <-time.After(grace):
			p.kill("did not stop in time")
		}
	}
	select {
	case <-p.done:
	case <-time.After(grace + 5*time.Second):
	}
}
