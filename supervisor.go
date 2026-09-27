package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------------
// BERMUDA Stealth Gateway NG — Supervisor Daemon
// Process Lifecycle, POSIX Termination & Adaptive Fast Loopback Probing
// ---------------------------------------------------------------------------

const (
	defaultXrayBin    = "/usr/local/bin/xray"
	defaultConfigPath = "/app/config.json"
	defaultAssetDir   = "/usr/local/share/xray"
	defaultXrayMemMB  = 640

	// Loopback backend addresses mapped directly to config.json inbounds
	defaultLoopbackXH = "127.0.0.1:18443"
	defaultLoopbackWS = "127.0.0.1:18444"
	defaultLoopbackTR = "127.0.0.1:18445"

	// Adaptive probing: fast cadence on startup, relaxing to conserve 2 vCPU budget
	fastProbeWindow     = 1500 * time.Millisecond
	fastProbeInterval   = 25 * time.Millisecond
	slowProbeInterval   = 100 * time.Millisecond
	probeDialTimeout    = 80 * time.Millisecond
	defaultStartTimeout = 10 * time.Second
	defaultStopTimeout  = 8 * time.Second

	stableWindow       = 60 * time.Second
	initialBackoff     = 80 * time.Millisecond
	maxBackoffInterval = 5 * time.Second
	backoffJitterFrac  = 0.20 // ±20% jitter prevents restart resonance storms
)

// scannerBufPool provides recycled 64KiB buffers for Xray stdout/stderr pumps,
// completely eliminating heap allocation churn during high log throughput.
var scannerBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 64*1024)
		return &b
	},
}

// SupervisorHealthSnapshot captures instantaneous telemetry across all 3 protocol inbounds.
type SupervisorHealthSnapshot struct {
	Running     bool   `json:"running"`
	Ready       bool   `json:"ready"`
	Restarts    int32  `json:"restarts"`
	UptimeSec   int64  `json:"uptime_sec"`
	XHInbound   bool   `json:"xh_inbound_ok"`
	WSInbound   bool   `json:"ws_inbound_ok"`
	TRInbound   bool   `json:"tr_inbound_ok"`
	LastProbeAt string `json:"last_probe_at"`
	ChildPID    int    `json:"child_pid,omitempty"`
}

// Supervisor manages the lifecycle, execution, telemetry, and graceful
// teardown of the Xray-core daemon process.
type Supervisor struct {
	binPath      string
	configPath   string
	assetDir     string
	memLimitMB   int
	xhAddr       string
	wsAddr       string
	trAddr       string
	startTimeout time.Duration
	stopTimeout  time.Duration

	mu        sync.Mutex
	cmd       *exec.Cmd
	startedAt time.Time

	running  atomic.Bool
	ready    atomic.Bool
	restarts atomic.Int32
	stopFlag atomic.Bool
	stopOnce atomic.Bool
	stopped  chan struct{}

	xhOk atomic.Bool
	wsOk atomic.Bool
	trOk atomic.Bool

	probeDialer *net.Dialer
}

// NewSupervisor initializes the supervisor with hardened production defaults
// and zero-wait probe dialers.
func NewSupervisor() *Supervisor {
	bin := getEnv("BERMUDA_XRAY_BIN", defaultXrayBin)
	cfg := getEnv("BERMUDA_XRAY_CONFIG", defaultConfigPath)
	assets := getEnv("XRAY_LOCATION_ASSET", defaultAssetDir)
	xh := getEnv("BERMUDA_BACKEND_XH", defaultLoopbackXH)
	ws := getEnv("BERMUDA_BACKEND_WS", defaultLoopbackWS)
	tr := getEnv("BERMUDA_BACKEND_TR", defaultLoopbackTR)

	// Probe dialer uses TCP_NODELAY and SO_LINGER 0 strictly on loopback probes.
	// This performs an abortive close on probe sockets, eliminating TIME_WAIT buildup.
	dialer := &net.Dialer{
		Timeout:   probeDialTimeout,
		KeepAlive: -1, // Disable keepalive; probe sockets are single-shot
		Control: func(network, address string, c syscall.RawConn) error {
			var ctrlErr error
			_ = c.Control(func(fd uintptr) {
				_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
				_ = syscall.SetsockoptLinger(int(fd), syscall.SOL_SOCKET, syscall.SO_LINGER, &syscall.Linger{Onoff: 1, Linger: 0})
			})
			return ctrlErr
		},
	}

	return &Supervisor{
		binPath:      bin,
		configPath:   cfg,
		assetDir:     assets,
		memLimitMB:   defaultXrayMemMB,
		xhAddr:       xh,
		wsAddr:       ws,
		trAddr:       tr,
		startTimeout: defaultStartTimeout,
		stopTimeout:  defaultStopTimeout,
		stopped:      make(chan struct{}),
		probeDialer:  dialer,
	}
}

// IsRunning reports whether the child Xray process is currently alive.
func (s *Supervisor) IsRunning() bool { return s.running.Load() }

// IsReady reports whether all 3 loopback inbounds (XHTTP, WS, Trojan) are accepting traffic.
func (s *Supervisor) IsReady() bool { return s.ready.Load() }

// Restarts returns the cumulative unexpected exit count.
func (s *Supervisor) Restarts() int32 { return s.restarts.Load() }

// Preflight executes a dry-run configuration syntax test before spawning.
func (s *Supervisor) Preflight() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, s.binPath, "run", "-test", "-c", s.configPath)
	cmd.Env = append(os.Environ(), fmt.Sprintf("XRAY_LOCATION_ASSET=%s", s.assetDir))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("xray preflight validation failed: %w, output: %s", err, strings.TrimSpace(string(out)))
	}
	log.Printf("[Supervisor] Preflight validation passed for %s", s.configPath)
	return nil
}

// Run executes the continuous supervision loop until ctx is canceled.
func (s *Supervisor) Run(ctx context.Context) error {
	defer close(s.stopped)

	backoff := initialBackoff

	for {
		if s.stopFlag.Load() || ctx.Err() != nil {
			return nil
		}

		err := s.startAndWait(ctx)
		if err == nil {
			return nil
		}

		if s.stopFlag.Load() || ctx.Err() != nil {
			return nil
		}

		s.restarts.Add(1)

		s.mu.Lock()
		uptime := time.Since(s.startedAt)
		s.mu.Unlock()

		if uptime >= stableWindow {
			backoff = initialBackoff
		}

		sleepFor := jitteredBackoff(backoff)
		log.Printf("[Supervisor] Xray exited unexpectedly after %s (err: %v). Scheduling restart (restarts=%d, backoff=%s)...",
			uptime.Round(time.Millisecond), err, s.restarts.Load(), sleepFor.Round(time.Millisecond))

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(sleepFor):
		}

		backoff *= 2
		if backoff > maxBackoffInterval {
			backoff = maxBackoffInterval
		}
	}
}

// jitteredBackoff applies ±20% randomized jitter using math/rand/v2 (thread-safe without mutex).
func jitteredBackoff(base time.Duration) time.Duration {
	if base <= 0 {
		return base
	}
	spread := float64(base) * backoffJitterFrac
	delta := (rand.Float64()*2 - 1) * spread
	result := time.Duration(float64(base) + delta)
	if result < 0 {
		result = 0
	}
	return result
}

// startAndWait spawns a single Xray child process and blocks until it exits.
func (s *Supervisor) startAndWait(ctx context.Context) error {
	s.mu.Lock()
	cmd := exec.Command(s.binPath, "run", "-c", s.configPath)

	// Linux Process Group isolation: SIGKILL propagates to child if PID 1 dies unexpectedly
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGKILL,
	}

	// Deterministic memory partitioning: 640MiB ceiling + GOGC=100 for optimal CPU efficiency
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("XRAY_LOCATION_ASSET=%s", s.assetDir),
		fmt.Sprintf("GOMEMLIMIT=%dMiB", s.memLimitMB),
		"GODEBUG=madvdontneed=1",
		"GOGC=100",
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("stdout pipe creation error: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("stderr pipe creation error: %w", err)
	}

	if err := cmd.Start(); err != nil {
		s.mu.Unlock()
		return fmt.Errorf("failed to start xray process: %w", err)
	}

	s.cmd = cmd
	s.startedAt = time.Now()
	pid := cmd.Process.Pid
	s.mu.Unlock()

	s.running.Store(true)
	s.ready.Store(false)
	log.Printf("[Supervisor] Xray child spawned (pid=%d, pgid=%d, GOMEMLIMIT=%dMiB, GOGC=100)", pid, pid, s.memLimitMB)

	// Stream stdout and stderr asynchronously using pooled recycled buffers
	go s.pumpPipe(stdout, "[Xray-Out]")
	go s.pumpPipe(stderr, "[Xray-Err]")

	// Trigger adaptive, low-latency loopback readiness verification
	go s.awaitReadiness(ctx)

	// Dedicated waitCh goroutine guarantees zero-zombie reaping
	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	select {
	case err := <-waitCh:
		s.mu.Lock()
		s.cmd = nil
		s.mu.Unlock()

		s.running.Store(false)
		s.ready.Store(false)
		s.xhOk.Store(false)
		s.wsOk.Store(false)
		s.trOk.Store(false)

		if s.stopFlag.Load() {
			log.Printf("[Supervisor] Xray child (pid=%d) terminated cleanly during shutdown", pid)
			return nil
		}
		return fmt.Errorf("xray child (pid=%d) terminated: %w", pid, err)

	case <-ctx.Done():
		s.stopChild(s.stopTimeout)
		<-waitCh // Structural reaping: guarantees zombie elimination
		s.mu.Lock()
		s.cmd = nil
		s.mu.Unlock()
		s.running.Store(false)
		s.ready.Store(false)
		return nil
	}
}

// pumpPipe reads lines from process pipes using pooled 64KiB buffers.
func (s *Supervisor) pumpPipe(r io.Reader, prefix string) {
	bufPtr := scannerBufPool.Get().(*[]byte)
	buf := *bufPtr
	defer scannerBufPool.Put(bufPtr)

	scanner := bufio.NewScanner(r)
	scanner.Buffer(buf, 256*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			log.Printf("%s %s", prefix, line)
		}
	}
}

// awaitReadiness polls loopback inbounds using fast-then-slow adaptive intervals.
func (s *Supervisor) awaitReadiness(ctx context.Context) {
	deadline := time.Now().Add(s.startTimeout)
	start := time.Now()

	for time.Now().Before(deadline) {
		interval := slowProbeInterval
		if time.Since(start) < fastProbeWindow {
			interval = fastProbeInterval
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}

		if !s.running.Load() {
			return
		}

		xhOK := s.probeTCP(s.xhAddr)
		wsOK := s.probeTCP(s.wsAddr)
		trOK := s.probeTCP(s.trAddr)

		s.xhOk.Store(xhOK)
		s.wsOk.Store(wsOK)
		s.trOk.Store(trOK)

		if xhOK && wsOK && trOK {
			s.ready.Store(true)
			log.Printf("[Supervisor] All 3 loopback inbounds verified ready in %s (XHTTP=%s, WS=%s, Trojan=%s)",
				time.Since(start).Round(time.Millisecond), s.xhAddr, s.wsAddr, s.trAddr)
			return
		}
	}

	log.Printf("[Supervisor] Warning: Readiness probe timed out after %s. Inbounds converging (xh=%t, ws=%t, tr=%t)",
		s.startTimeout, s.xhOk.Load(), s.wsOk.Load(), s.trOk.Load())
}

// probeTCP performs an isolated dial test with TCP_NODELAY and SO_LINGER 0.
func (s *Supervisor) probeTCP(addr string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), probeDialTimeout)
	defer cancel()

	conn, err := s.probeDialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	_ = conn.Close() // Immediate RST close, zero TIME_WAIT accumulation
	return true
}

// ProbeNow executes an active on-demand health check across all 3 inbounds for /healthz.
func (s *Supervisor) ProbeNow() (bool, bool, bool) {
	if !s.running.Load() {
		return false, false, false
	}
	xh := s.probeTCP(s.xhAddr)
	ws := s.probeTCP(s.wsAddr)
	tr := s.probeTCP(s.trAddr)
	s.xhOk.Store(xh)
	s.wsOk.Store(ws)
	s.trOk.Store(tr)
	return xh, ws, tr
}

// Snapshot gathers instantaneous health status atomically across all inbounds.
func (s *Supervisor) Snapshot() SupervisorHealthSnapshot {
	s.mu.Lock()
	started := s.startedAt
	var pid int
	if s.cmd != nil && s.cmd.Process != nil {
		pid = s.cmd.Process.Pid
	}
	s.mu.Unlock()

	var uptime int64
	if s.running.Load() && !started.IsZero() {
		uptime = int64(time.Since(started).Seconds())
	}

	return SupervisorHealthSnapshot{
		Running:     s.running.Load(),
		Ready:       s.ready.Load(),
		Restarts:    s.restarts.Load(),
		UptimeSec:   uptime,
		XHInbound:   s.xhOk.Load(),
		WSInbound:   s.wsOk.Load(),
		TRInbound:   s.trOk.Load(),
		LastProbeAt: time.Now().UTC().Format(time.RFC3339),
		ChildPID:    pid,
	}
}

// stopChild sends SIGTERM to the process group, verifies exit via POSIX kill(pid, 0),
// and escalates to SIGKILL strictly if the grace period is exceeded.
func (s *Supervisor) stopChild(timeout time.Duration) {
	if !s.stopOnce.CompareAndSwap(false, true) {
		return
	}
	s.stopFlag.Store(true)

	s.mu.Lock()
	cmd := s.cmd
	s.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return
	}

	pid := cmd.Process.Pid
	log.Printf("[Supervisor] Initiating graceful termination for process group -%d (timeout=%s)", pid, timeout)

	// Send SIGTERM to the entire process group
	_ = syscall.Kill(-pid, syscall.SIGTERM)

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		// POSIX standard non-destructive liveness test: kill(pid, 0)
		if err := syscall.Kill(-pid, 0); err != nil {
			log.Printf("[Supervisor] Process group -%d exited gracefully via SIGTERM", pid)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	log.Printf("[Supervisor] Grace period exceeded. Escalating to SIGKILL on process group -%d", pid)
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

// Stop initiates child termination and blocks until the supervisor loop finishes.
func (s *Supervisor) Stop(timeout time.Duration) {
	s.stopChild(timeout)
	select {
	case <-s.stopped:
	case <-time.After(timeout + 2*time.Second):
	}
}

// getEnv retrieves environment variables with safe fallback.
func getEnv(key, fallback string) string {
	if val := strings.TrimSpace(os.Getenv(key)); val != "" {
		return val
	}
	return fallback
}
