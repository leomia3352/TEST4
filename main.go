package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strconv"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------------
// BERMUDA Stealth Gateway NG — Edge Entrypoint & Runtime Orchestrator
// PID 1 Supervisor Integration, TCP_NODELAY Listener & Ordered 3-Stage Teardown
// ---------------------------------------------------------------------------

const (
	defaultPort        = "8080"
	defaultSelfMemMB   = 128
	defaultGOMAXPROCS  = 2
	httpDrainTimeout   = 10 * time.Second
	supervisorStopWait = 8 * time.Second
)

// applyMemoryCeiling enforces a hard runtime soft-limit on heap allocations
// for the Go gateway process (128MiB), leaving the remainder of the 1GiB
// instance budget to the Xray daemon (640MiB) and Linux kernel socket buffers (~256MiB).
func applyMemoryCeiling(selfMB int) {
	debug.SetMemoryLimit(int64(selfMB) << 20)
	debug.SetGCPercent(100) // Standard GC pacing paired with soft memory ceiling prevents CPU burn on 2 vCPUs
}

// applyGOMAXPROCS pins the Go scheduler to match Railway's provisioned 2 vCPU quota.
// Critical Go 1.24 fix: Go 1.24 is not cgroup-aware and defaults to host CPUs (64-128 cores),
// which triggers severe CPU throttling unless explicitly pinned to the container quota.
func applyGOMAXPROCS() int {
	n := defaultGOMAXPROCS
	if v := os.Getenv("BERMUDA_GOMAXPROCS"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			n = parsed
		}
	}
	runtime.GOMAXPROCS(n)
	return n
}

// tcpKeepAliveListener injects TCP_NODELAY and keep-alive directly on incoming sockets.
// Critical: Disables Nagle's algorithm to eliminate up to 40ms delayed-ACK stalls,
// while strictly avoiding SO_LINGER 0 to preserve graceful FIN teardowns for client tunnels.
type tcpKeepAliveListener struct {
	*net.TCPListener
}

func (ln tcpKeepAliveListener) Accept() (net.Conn, error) {
	tc, err := ln.AcceptTCP()
	if err != nil {
		return nil, err
	}
	// Enable keepalive with 30s period to maintain NAT/CGNAT state across mobile cellular flutter
	_ = tc.SetKeepAlive(true)
	_ = tc.SetKeepAlivePeriod(30 * time.Second)
	// TCP_NODELAY eliminates Nagle packet coalescing for real-time streaming
	_ = tc.SetNoDelay(true)
	return tc, nil
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	log.Println("[Gateway] Initializing BERMUDA Stealth Gateway NG...")

	// 1. Enforce memory constraints and pin scheduler to 2 vCPUs
	applyMemoryCeiling(defaultSelfMemMB)
	procs := applyGOMAXPROCS()
	log.Printf("[Runtime] Gateway memory ceiling locked at %dMiB (GOGC=100), GOMAXPROCS=%d", defaultSelfMemMB, procs)

	port := getEnv("PORT", defaultPort)

	// 2. Instantiate and preflight validate Xray daemon supervisor
	sup := NewSupervisor()
	if err := sup.Preflight(); err != nil {
		log.Printf("[Gateway] Warning: Supervisor preflight validation issue: %v. Continuing to start...", err)
	}

	// 3. Instantiate zero-buffer streaming reverse proxy gateway with 32KiB pool
	gw := NewGateway(sup)

	// 4. Capture container lifecycle termination signals (SIGTERM from Railway / SIGINT)
	rootCtx, stopRoot := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stopRoot()

	// 5. Run supervisor loop in a dedicated background goroutine
	supErrCh := make(chan error, 1)
	go func() {
		supErrCh <- sup.Run(rootCtx)
	}()

	// 6. Configure HTTP edge server with line-rate socket tuning
	// Note: ReadTimeout and WriteTimeout are deliberately OMITTED.
	// VLESS XHTTP, WebSocket, and Trojan tunnels are long-lived continuous bidirectional
	// streams that must never be severed by intermediate server deadlines.
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           gw.Handler(),
		ReadHeaderTimeout: 5 * time.Second,  // Protects against Slowloris header trickle attacks
		IdleTimeout:       120 * time.Second, // Matches loopback transport idle timeout
		MaxHeaderBytes:    32 * 1024,
		ConnState:         gw.TrackConnState,
	}

	// Manually bind TCP listener to inject TCP_NODELAY socket options
	ln, err := net.Listen("tcp", ":"+port)
	if err != nil {
		log.Fatalf("[Gateway] Fatal: Failed to bind edge listener :%s: %v", port, err)
	}

	tcpListener := &tcpKeepAliveListener{
		TCPListener: ln.(*net.TCPListener),
	}

	serverErrCh := make(chan error, 1)
	go func() {
		log.Printf("[Gateway] Edge listener active on :%s (PID %d, GOMAXPROCS=%d, GOMEMLIMIT=%dMiB, TCP_NODELAY=true)",
			port, os.Getpid(), procs, defaultSelfMemMB)
		if err := srv.Serve(tcpListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrCh <- err
		}
	}()

	// 7. Await termination signal or unexpected fatal failures
	select {
	case err := <-serverErrCh:
		log.Printf("[Gateway] Fatal: HTTP server failure: %v", err)
		gw.SetDraining()
		sup.Stop(supervisorStopWait)
		os.Exit(1)

	case err := <-supErrCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("[Gateway] Fatal: Supervisor loop halted unexpectedly: %v", err)
			gw.SetDraining()
			sup.Stop(supervisorStopWait)
			os.Exit(1)
		}

	case <-rootCtx.Done():
		log.Println("[Gateway] Termination signal (SIGTERM/SIGINT) intercepted. Commencing graceful drain sequence...")
	}

	// ---------------------------------------------------------------------------
	// Graceful Drain State Machine (Ordered Zero-Downtime Teardown)
	// ---------------------------------------------------------------------------

	// Stage 1: Immediately flip /healthz to 503 so Railway's edge mesh sheds incoming traffic
	gw.SetDraining()
	log.Println("[Gateway] Stage 1/3: /healthz flipped to 503 (draining active traffic)")

	// Stage 2: Allow in-flight connections to drain cleanly up to httpDrainTimeout
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), httpDrainTimeout)
	defer cancelDrain()

	if err := srv.Shutdown(drainCtx); err != nil {
		log.Printf("[Gateway] Stage 2/3 Warning: HTTP server drain timeout exceeded: %v. Forcing socket closure.", err)
		_ = srv.Close()
	} else {
		log.Println("[Gateway] Stage 2/3: All edge HTTP connections drained successfully.")
	}

	// Stage 2.5: Proactively release pooled loopback sockets before child termination
	gw.CloseIdleBackendConns()

	// Stage 3: Stop child process group cleanly and reap zombie state
	log.Println("[Gateway] Stage 3/3: Teardown child Xray process group...")
	sup.Stop(supervisorStopWait)

	log.Println("[Gateway] BERMUDA Stealth Gateway shutdown complete. Ports released cleanly. Exit 0.")
}
