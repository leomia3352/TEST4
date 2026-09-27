package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------------
// BERMUDA Stealth Gateway NG — L7 Streaming Reverse Proxy & Camouflage Shield
// 32KiB Zero-Allocation Buffer Pool, Churn-Free Transport & OpenResty Emulation
// ---------------------------------------------------------------------------

const (
	defaultPathXH = "/bermuda-xhttp"
	defaultPathWS = "/bermuda-ws"
	defaultPathTR = "/bermuda-tr"

	openrestyServerToken   = "openresty"
	openrestyStaticLastMod = "Tue, 28 May 2024 12:00:00 GMT"
	openrestyIndexETag     = "\"6655c680-264\""
	proxyBufferSize        = 32 * 1024 // 32 KiB matches io.Copy default; zero heap fragmentation
)

// Authentic OpenResty HTML Bodies matching official distribution templates byte-for-byte
const (
	openrestyWelcomeHTML = "<!DOCTYPE html>\r\n" +
		"<html>\r\n" +
		"<head>\r\n" +
		"<title>Welcome to OpenResty!</title>\r\n" +
		"<style>\r\n" +
		"body {\r\n" +
		"    width: 35em;\r\n" +
		"    margin: 0 auto;\r\n" +
		"    font-family: Tahoma, Verdana, Arial, sans-serif;\r\n" +
		"}\r\n" +
		"</style>\r\n" +
		"</head>\r\n" +
		"<body>\r\n" +
		"<h1>Welcome to OpenResty!</h1>\r\n" +
		"<p>If you see this page, the OpenResty web platform is successfully installed and\r\n" +
		"working. Further configuration is required.</p>\r\n" +
		"\r\n" +
		"<p>For online documentation and support please refer to\r\n" +
		"<a href=\"https://openresty.org/\">openresty.org</a>.<br/>\r\n" +
		"Commercial support is available at\r\n" +
		"<a href=\"https://openresty.com/\">openresty.com</a>.</p>\r\n" +
		"\r\n" +
		"<p><em>Thank you for flying OpenResty.</em></p>\r\n" +
		"</body>\r\n" +
		"</html>\r\n"

	openresty404HTML = "<html>\r\n" +
		"<head><title>404 Not Found</title></head>\r\n" +
		"<body>\r\n" +
		"<center><h1>404 Not Found</h1></center>\r\n" +
		"<hr><center>openresty</center>\r\n" +
		"</body>\r\n" +
		"</html>\r\n"

	openresty403HTML = "<html>\r\n" +
		"<head><title>403 Forbidden</title></head>\r\n" +
		"<body>\r\n" +
		"<center><h1>403 Forbidden</h1></center>\r\n" +
		"<hr><center>openresty</center>\r\n" +
		"</body>\r\n" +
		"</html>\r\n"

	openresty405HTML = "<html>\r\n" +
		"<head><title>405 Not Allowed</title></head>\r\n" +
		"<body>\r\n" +
		"<center><h1>405 Not Allowed</h1></center>\r\n" +
		"<hr><center>openresty</center>\r\n" +
		"</body>\r\n" +
		"</html>\r\n"

	openresty502HTML = "<html>\r\n" +
		"<head><title>502 Bad Gateway</title></head>\r\n" +
		"<body>\r\n" +
		"<center><h1>502 Bad Gateway</h1></center>\r\n" +
		"<hr><center>openresty</center>\r\n" +
		"</body>\r\n" +
		"</html>\r\n"
)

// ---------------------------------------------------------------------------
// Zero-Allocation Buffer Pool (httputil.BufferPool interface)
// ---------------------------------------------------------------------------

type recycledBufferPool struct {
	pool sync.Pool
}

func newRecycledBufferPool() *recycledBufferPool {
	return &recycledBufferPool{
		pool: sync.Pool{
			New: func() any {
				b := make([]byte, proxyBufferSize)
				return &b
			},
		},
	}
}

func (p *recycledBufferPool) Get() []byte {
	bptr := p.pool.Get().(*[]byte)
	return *bptr
}

func (p *recycledBufferPool) Put(b []byte) {
	if cap(b) != proxyBufferSize {
		return
	}
	b = b[:cap(b)]
	p.pool.Put(&b)
}

// ---------------------------------------------------------------------------
// Telemetry & Health Models
// ---------------------------------------------------------------------------

type HealthResponse struct {
	Status     string                   `json:"status"`
	Draining   bool                     `json:"draining"`
	Healthy    bool                     `json:"healthy"`
	UptimeSec  int64                    `json:"uptime_sec"`
	Supervisor SupervisorHealthSnapshot `json:"supervisor"`
	Telemetry  TelemetrySnapshot        `json:"telemetry"`
}

type TelemetrySnapshot struct {
	OpenConnections   int64 `json:"open_connections"`
	TunnelConnections int64 `json:"tunnel_connections"`
	TotalRequests     int64 `json:"total_requests"`
}

// Gateway encapsulates edge reverse-proxy router, connection trackers,
// health endpoints, and the active camouflage decoy layer across all 3 protocols.
type Gateway struct {
	sup       *Supervisor
	pathXH    string
	pathWS    string
	pathTR    string
	backendXH string
	backendWS string
	backendTR string

	xhProxy *httputil.ReverseProxy
	wsProxy *httputil.ReverseProxy
	trProxy *httputil.ReverseProxy

	tr       *http.Transport
	bufPool  *recycledBufferPool
	draining atomic.Bool

	openConns   atomic.Int64
	tunnelConns atomic.Int64
	totalReq    atomic.Int64
	startedAt   time.Time
}

// NewGateway constructs and configures the streaming reverse proxy gateway
// multiplexing VLESS-XHTTP, VLESS-WS, and Trojan-WS over Railway's single exposed port.
func NewGateway(sup *Supervisor) *Gateway {
	pathXH := getEnv("BERMUDA_PATH_XH", defaultPathXH)
	pathWS := getEnv("BERMUDA_PATH_WS", defaultPathWS)
	pathTR := getEnv("BERMUDA_PATH_TR", defaultPathTR)

	backendXH := getEnv("BERMUDA_BACKEND_XH", defaultLoopbackXH)
	backendWS := getEnv("BERMUDA_BACKEND_WS", defaultLoopbackWS)
	backendTR := getEnv("BERMUDA_BACKEND_TR", defaultLoopbackTR)

	sharedPool := newRecycledBufferPool()
	tr := newLoopbackTransport()

	gw := &Gateway{
		sup:         sup,
		pathXH:      pathXH,
		pathWS:      pathWS,
		pathTR:      pathTR,
		backendXH:   backendXH,
		backendWS:   backendWS,
		backendTR:   backendTR,
		xhProxy:     newBackendProxy(backendXH, tr, sharedPool),
		wsProxy:     newBackendProxy(backendWS, tr, sharedPool),
		trProxy:     newBackendProxy(backendTR, tr, sharedPool),
		tr:          tr,
		bufPool:     sharedPool,
		startedAt:   time.Now(),
	}

	return gw
}

// newLoopbackTransport fixes Go's default MaxIdleConnsPerHost: 2 bottleneck!
// Tuned for high-concurrency multi-connection bursts without socket churn.
func newLoopbackTransport() *http.Transport {
	dialer := &net.Dialer{
		Timeout:   3 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(network, address string, c syscall.RawConn) error {
			var err error
			_ = c.Control(func(fd uintptr) {
				// TCP_NODELAY on loopback to ensure immediate handoff
				_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
			})
			return err
		},
	}

	return &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          512,               // Shared idle pool across 3 backends
		MaxIdleConnsPerHost:   256,               // Fixes Go's default 2 socket churn bug!
		MaxConnsPerHost:       512,               // Ample headroom for concurrent multi-thread IDM bursts
		IdleConnTimeout:       90 * time.Second,
		DisableCompression:    true,              // Avoid CPU burn on already-encrypted payloads
		ReadBufferSize:        32 * 1024,         // 32 KB
		WriteBufferSize:       32 * 1024,         // 32 KB
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// newBackendProxy instantiates a ReverseProxy configured for zero-latency streaming.
// FlushInterval: -1 guarantees immediate, unbuffered chunk propagation for XHTTP
// and transparent socket hijacking for WebSocket 101 Switching Protocols.
func newBackendProxy(targetAddr string, tr http.RoundTripper, bp httputil.BufferPool) *httputil.ReverseProxy {
	targetURL, _ := url.Parse("http://" + targetAddr)

	return &httputil.ReverseProxy{
		Transport:     tr,
		BufferPool:    bp,
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = targetURL.Scheme
			pr.Out.URL.Host = targetURL.Host
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
			pr.Out.Header.Set("X-Accel-Buffering", "no")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("[Proxy Error] Backend %s unreachable: %v", targetAddr, err)
			camoOpenResty(w, r, http.StatusBadGateway, openresty502HTML, nil)
		},
	}
}

func (g *Gateway) SetDraining() {
	g.draining.Store(true)
}

func (g *Gateway) IsDraining() bool {
	return g.draining.Load()
}

func (g *Gateway) CloseIdleBackendConns() {
	if g.tr != nil {
		g.tr.CloseIdleConnections()
	}
}

func (g *Gateway) TrackConnState(_ net.Conn, state http.ConnState) {
	switch state {
	case http.StateNew:
		g.openConns.Add(1)
	case http.StateClosed:
		g.openConns.Add(-1)
	case http.StateHijacked:
		g.openConns.Add(-1)
		g.tunnelConns.Add(1)
	}
}

// Handler returns the primary HTTP multiplexer implementing hot-path bypass,
// multi-protocol routing (VLESS-XHTTP, VLESS-WS, Trojan-WS), and camouflage decoys.
func (g *Gateway) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.totalReq.Add(1)

		// 1. Healthcheck Endpoint (Railway Platform Orchestration Probe)
		if r.URL.Path == "/healthz" {
			g.handleHealthz(w, r)
			return
		}

		// 2. VLESS-XHTTP Hot-Path (Zero-buffer line-rate streaming)
		if r.URL.Path == g.pathXH || strings.HasPrefix(r.URL.Path, g.pathXH+"/") {
			g.xhProxy.ServeHTTP(w, r)
			return
		}

		// 3. VLESS-WS Hot-Path (Pass native unwrapped ResponseWriter for 101 Switching Protocols)
		if r.URL.Path == g.pathWS || strings.HasPrefix(r.URL.Path, g.pathWS+"/") {
			g.wsProxy.ServeHTTP(w, r)
			return
		}

		// 4. Trojan-WS Hot-Path (WebSocket / HTTP Trojan ingress)
		if r.URL.Path == g.pathTR || strings.HasPrefix(r.URL.Path, g.pathTR+"/") {
			g.trProxy.ServeHTTP(w, r)
			return
		}

		// 5. Camouflage & Active Probe Neutralization Layer
		g.serveCamouflage(w, r)
	})
}

// serveCamouflage handles passive scanners, network probes, and non-tunnel web traffic,
// mimicking an authentic OpenResty / Nginx reverse proxy down to exact RFC headers.
func (g *Gateway) serveCamouflage(w http.ResponseWriter, r *http.Request) {
	cleanPath := path.Clean(r.URL.Path)

	// Enforce HTTP method permissions for standard benign web servers
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodPost {
		camoOpenResty(w, r, http.StatusMethodNotAllowed, openresty405HTML, map[string]string{
			"Allow": "GET, HEAD, POST",
		})
		return
	}

	// Root path and standard index: Authentic OpenResty Welcome Page
	if cleanPath == "/" || cleanPath == "/index.html" {
		camoOpenResty(w, r, http.StatusOK, openrestyWelcomeHTML, map[string]string{
			"Last-Modified": openrestyStaticLastMod,
			"ETag":          openrestyIndexETag,
			"Accept-Ranges": "bytes",
		})
		return
	}

	// Scanner probing on hidden dotfiles (e.g., /.env, /.git, /.svn, /.DS_Store)
	if strings.HasPrefix(cleanPath, "/.") {
		camoOpenResty(w, r, http.StatusForbidden, openresty403HTML, nil)
		return
	}

	// Common active scanner probe targets (/robots.txt, /favicon.ico, /wp-login.php, /wp-admin, etc.)
	camoOpenResty(w, r, http.StatusNotFound, openresty404HTML, nil)
}

func (g *Gateway) handleHealthz(w http.ResponseWriter, r *http.Request) {
	snap := g.sup.Snapshot()
	draining := g.draining.Load()

	healthy := !draining && snap.Running && snap.Ready
	statusStr := "ok"

	code := http.StatusOK
	if draining {
		code = http.StatusServiceUnavailable
		statusStr = "draining"
	} else if !snap.Running {
		code = http.StatusServiceUnavailable
		statusStr = "down"
	} else if !snap.Ready {
		code = http.StatusServiceUnavailable
		statusStr = "starting"
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.WriteHeader(code)

	payload := HealthResponse{
		Status:     statusStr,
		Draining:   draining,
		Healthy:    healthy,
		UptimeSec:  int64(time.Since(g.startedAt).Seconds()),
		Supervisor: snap,
		Telemetry: TelemetrySnapshot{
			OpenConnections:   g.openConns.Load(),
			TunnelConnections: g.tunnelConns.Load(),
			TotalRequests:     g.totalReq.Load(),
		},
	}

	_ = json.NewEncoder(w).Encode(payload)
}

// camoOpenResty writes an authentic OpenResty response:
//   - Dynamic RFC 1123 GMT Date header
//   - Authentic Server: openresty token
//   - Exact Content-Length matching body byte length
//   - Standard Content-Type: text/html
//   - Accurate Connection state management (keep-alive vs close)
//   - Full support for HEAD requests (headers emitted, body suppressed)
func camoOpenResty(w http.ResponseWriter, r *http.Request, statusCode int, body string, extraHeaders map[string]string) {
	bodyBytes := []byte(body)

	// 1. Mandatory OpenResty server tokens & dynamic RFC 1123 Date header
	w.Header().Set("Server", openrestyServerToken)
	w.Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
	w.Header().Set("Content-Type", "text/html")
	w.Header().Set("Content-Length", strconv.Itoa(len(bodyBytes)))

	// 2. Client Connection Header Alignment
	connReq := strings.ToLower(r.Header.Get("Connection"))
	if connReq == "close" || r.Close {
		w.Header().Set("Connection", "close")
	} else {
		w.Header().Set("Connection", "keep-alive")
	}

	// 3. Inject route-specific static headers (ETag, Last-Modified, Allow)
	for k, v := range extraHeaders {
		w.Header().Set(k, v)
	}

	// 4. Emit HTTP status code
	w.WriteHeader(statusCode)

	// 5. If HEAD request, terminate without emitting body bytes per RFC 7231
	if r.Method == http.MethodHead {
		return
	}

	// 6. Write byte-exact HTML body
	_, _ = w.Write(bodyBytes)
}

// ---------------------------------------------------------------------------
// Transparent ResponseWriter Interface Wrappers
// ---------------------------------------------------------------------------

type tunnelResponseWriter struct {
	http.ResponseWriter
	statusCode  int
	wroteHeader bool
}

func (w *tunnelResponseWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.statusCode = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *tunnelResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.statusCode = http.StatusOK
		w.wroteHeader = true
	}
	return w.ResponseWriter.Write(b)
}

func (w *tunnelResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *tunnelResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, errors.New("underlying ResponseWriter does not support Hijack")
}

func (w *tunnelResponseWriter) ReadFrom(r io.Reader) (int64, error) {
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		if !w.wroteHeader {
			w.statusCode = http.StatusOK
			w.wroteHeader = true
		}
		return rf.ReadFrom(r)
	}
	return io.Copy(w.ResponseWriter, r)
}
