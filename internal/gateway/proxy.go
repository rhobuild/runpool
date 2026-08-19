package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rhobuild/runpool/internal/egress"
)

// Bounds on the relay. Every one of them exists because the work is
// driven by a workload: without them a job could spend the host's file
// descriptors, memory and goroutines from outside its own cgroup.
const (
	// MaxProxyConnections is the number of concurrent client
	// connections the relay will serve. Beyond it, new connections are
	// refused rather than queued — a CI job that needs more than this
	// many simultaneous egress connections is pathological, and
	// queueing would only convert refusal into latency plus memory.
	MaxProxyConnections = 256
	// MaxHeaderBytes bounds one request's headers.
	MaxHeaderBytes = 64 << 10
	// ReadHeaderTimeout bounds how long a client may take to send its
	// request line and headers — the slowloris bound.
	ReadHeaderTimeout = 15 * time.Second
	// IdleTimeout closes a connection that goes quiet, so an abandoned
	// tunnel does not hold a slot forever.
	IdleTimeout = 2 * time.Minute
	// TunnelIdleTimeout bounds silence inside an established CONNECT
	// tunnel. It is generous: a long docker pull is legitimately quiet
	// between chunks, and cutting those would break real work.
	TunnelIdleTimeout = 10 * time.Minute
	// DialTimeout bounds establishing an upstream connection.
	DialTimeout = 30 * time.Second
	// MaxUpstreamConns bounds the pooled transport used for plain HTTP.
	MaxUpstreamConns = 128
)

// AllowedConnectPorts is the explicit egress port set, applied to every
// destination the relay dials — CONNECT tunnels and absolute-URI plain
// HTTP alike. The name kept its CONNECT origin; the rule did not.
//
// This is the honest statement of the relay's contract. Direct TCP from
// a capsule cannot leave — the host drops it — but CONNECT is a TCP
// tunnel, so a proxy-aware client can carry any protocol over it to an
// allowed address. Restricting the port set is what keeps that from
// silently being "arbitrary TCP egress to the public internet": these
// are the ports CI actually needs, and anything else is refused with a
// reason. The relay does not inspect what flows inside a tunnel and
// does not claim to.
var AllowedConnectPorts = map[int]string{
	443: "HTTPS",
	80:  "HTTP",
}

// hopByHop are the headers a proxy must not forward. RFC 9110 §7.6.1:
// they describe the single-hop connection, not the message, and passing
// them on is how request smuggling and connection confusion start.
var hopByHop = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"TE",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// Relay is the capsule's only way out: an HTTP proxy that applies the
// address policy to every destination it is asked for. CONNECT carries
// TLS and anything else tunnelled over it; absolute-URI requests carry
// plain HTTP. In both cases the relay resolves the name itself and
// dials only an address the policy allows, so the capsule never
// influences which address a connection actually reaches — and a
// resolver answering with a private address (DNS rebinding) changes
// nothing, because the address that was checked is the address dialed.
type Relay struct {
	Policy *PolicyStore
	Log    *slog.Logger

	once      sync.Once
	transport atomic.Pointer[http.Transport]
	// policyGen is the policy generation the pooled connections were
	// authorised under.
	policyGen atomic.Uint64
}

// Listen starts the relay on the internal leg.
func (r *Relay) Listen(ctx context.Context, ip string) error {
	ln, err := net.Listen("tcp", net.JoinHostPort(ip, strconv.Itoa(egress.ProxyPort)))
	if err != nil {
		return err
	}
	return r.serve(ctx, ln)
}

func (r *Relay) serve(ctx context.Context, ln net.Listener) error {
	// The connection quota is enforced at accept, before any per-request
	// work exists: a refused connection costs one accept and one close.
	limited := &limitListener{Listener: ln, sem: make(chan struct{}, MaxProxyConnections)}
	srv := &http.Server{
		Handler:           r,
		ReadHeaderTimeout: ReadHeaderTimeout,
		IdleTimeout:       IdleTimeout,
		MaxHeaderBytes:    MaxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(r.Log.Handler(), slog.LevelDebug),
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	go func() {
		if err := srv.Serve(limited); err != nil && !errors.Is(err, http.ErrServerClosed) {
			r.Log.Error("egress relay stopped", "error", err)
		}
	}()
	return nil
}

// ServeHTTP dispatches the two proxy shapes: CONNECT tunnels and
// absolute-URI plain HTTP. A request that is neither is not a proxy
// request, and this server has nothing else to serve.
func (r *Relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodConnect {
		r.connect(w, req)
		return
	}
	if !req.URL.IsAbs() {
		http.Error(w, "runpool egress relay: only proxy requests are served", http.StatusBadRequest)
		return
	}
	r.forward(w, req)
}

// connect tunnels a TCP stream after the policy check. The authority is
// parsed strictly — a CONNECT target is host:port and nothing else, and
// accepting anything looser is how a proxy is talked into connecting
// somewhere its policy never saw.
func (r *Relay) connect(w http.ResponseWriter, req *http.Request) {
	host, port, err := parseAuthority(req.Host)
	if err != nil {
		http.Error(w, "runpool egress relay: "+err.Error(), http.StatusBadRequest)
		return
	}
	upstream, err := r.dial(req.Context(), host, port)
	if err != nil {
		r.refuse(w, err)
		return
	}
	defer upstream.Close()

	r.establishTunnel(w, upstream)
}

// establishTunnel answers the CONNECT and joins the two streams. It is
// separated from the policy decision above because joining is the half
// that can be tested without a network, and it is the half where bytes
// can go missing.
func (r *Relay) establishTunnel(w http.ResponseWriter, upstream net.Conn) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "runpool egress relay: cannot tunnel", http.StatusInternalServerError)
		return
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer client.Close()
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	// Whatever the client pipelined behind its CONNECT header block is
	// already out of the socket and inside the server's reader: parsing
	// the headers reads by the block, not by the byte, and a client that
	// does not wait for the 200 — legal, and the dial above gives it up
	// to a minute to happen — has its first payload sitting there. Read
	// from the raw connection and those bytes are simply gone: the
	// upstream never sees a ClientHello, the client waits for a
	// ServerHello, and the tunnel holds a connection slot until the idle
	// timeout ten minutes later.
	if n := buffered.Reader.Buffered(); n > 0 {
		_ = upstream.SetWriteDeadline(time.Now().Add(TunnelIdleTimeout))
		if _, err := io.CopyN(upstream, buffered.Reader, int64(n)); err != nil {
			return
		}
		_ = upstream.SetWriteDeadline(time.Time{})
	}
	tunnel(client, upstream)
}

// closeWriter is the half-close a tunnel propagates. Every connection a
// tunnel joins has it; naming the interface is what lets a wrapper that
// hides the method be given one back rather than silently falling
// through to a full close.
type closeWriter interface{ CloseWrite() error }

// tunnelPollInterval is how often a quiet direction re-checks the
// tunnel's shared activity clock. It also bounds how far past the idle
// timeout a genuinely silent tunnel can live.
const tunnelPollInterval = 30 * time.Second

// tunnel copies both directions and returns when both of them have
// ended.
//
// A clean EOF on one direction is a half-close, not the end of the
// tunnel. Propagating it as CloseWrite is what lets the peer finish its
// own reply; returning there truncated the response of every client
// that shuts down its write side once its request is out — an ordinary
// thing for an HTTP client to do, and the reason both directions are
// now waited on: the deferred closes around this call fire when it
// returns, so ending early closes the connection the peer was still
// answering on.
//
// The idle bound is the tunnel's, not each direction's. A long one-way
// transfer is silent in the other direction for its whole duration, so
// a per-direction deadline severed a large upload or a slow download
// while it was actively streaming. Each direction now reads in short
// intervals and consults a clock both of them write.
func tunnel(client, upstream net.Conn) {
	tunnelWith(client, upstream, TunnelIdleTimeout, tunnelPollInterval)
}

func tunnelWith(client, upstream net.Conn, idle, poll time.Duration) {
	// One clock for the tunnel: activity in either direction is what
	// keeps the other alive.
	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())

	var wg sync.WaitGroup
	copyIdle := func(dst, src net.Conn) {
		defer wg.Done()
		buf := make([]byte, 32<<10)
		for {
			_ = src.SetReadDeadline(time.Now().Add(poll))
			n, err := src.Read(buf)
			if n > 0 {
				lastActivity.Store(time.Now().UnixNano())
				_ = dst.SetWriteDeadline(time.Now().Add(idle))
				if _, werr := dst.Write(buf[:n]); werr != nil {
					break
				}
				lastActivity.Store(time.Now().UnixNano())
			}
			switch {
			case err == nil:
				continue
			case errors.Is(err, os.ErrDeadlineExceeded):
				// Quiet here. The tunnel is idle only if it is quiet
				// both ways, which is what the shared clock answers.
				if time.Since(time.Unix(0, lastActivity.Load())) < idle {
					continue
				}
			case errors.Is(err, io.EOF):
				// This side is done sending and says so, leaving the
				// peer to finish. The tunnel ends when that one ends
				// too.
				if cw, ok := dst.(closeWriter); ok {
					_ = cw.CloseWrite()
					return
				}
			}
			break
		}
		// The tunnel is over rather than half-closed: unblock the peer
		// instead of leaving its goroutine parked on a read.
		_ = src.SetReadDeadline(time.Now())
		_ = dst.SetReadDeadline(time.Now())
	}
	wg.Add(2)
	go copyIdle(upstream, client)
	go copyIdle(client, upstream)
	wg.Wait()
}

// forward relays a plain HTTP request through a shared, bounded
// transport. Hop-by-hop headers are stripped in both directions: they
// describe a single connection, and a proxy that forwards them invites
// request smuggling.
func (r *Relay) forward(w http.ResponseWriter, req *http.Request) {
	// Validate the authority here, where a bad one can still be
	// answered with 400. The transport's dialer parses it again because
	// that is where the connection is actually made, and the check that
	// matters is the one next to the dial.
	if _, _, err := parseAuthority(defaultPort(req.URL.Host, req.URL.Scheme)); err != nil {
		http.Error(w, "runpool egress relay: "+err.Error(), http.StatusBadRequest)
		return
	}
	outbound := req.Clone(req.Context())
	outbound.RequestURI = ""
	stripHopByHop(outbound.Header)

	// The check runs first and the transport is read after it: the check
	// can replace the transport, and reading it beforehand sends this
	// request through the pool that was just retired.
	r.expireStaleConnections()
	resp, err := r.roundTripper().RoundTrip(outbound)
	if err != nil {
		r.refuse(w, err)
		return
	}
	defer resp.Body.Close()
	stripHopByHop(resp.Header)
	for k, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// expireStaleConnections retires the pool once the policy moves.
//
// The address check lives in the transport's dialer, and http.Transport
// only dials when it has no connection for the destination — so a
// restriction installed while a job kept a connection warm would not
// bind to its next request. Closing the idle connections is not enough
// to fix that: by contract it leaves a connection carrying a request
// alone, and that connection returns to the pool afterwards, where the
// next request reuses it without ever reaching the dialer. The kernel
// does not catch it either, because the ruleset accepts established
// traffic ahead of every reject. A revoked address therefore stayed
// reachable for as long as a job kept using it.
//
// Replacing the transport is what makes the old pool unreachable:
// nothing routes through it again, whatever state its connections were
// in. The ones already carrying a request finish it and go with it.
func (r *Relay) expireStaleConnections() {
	if r.Policy == nil {
		return
	}
	// Current() is what notices the file changed, and it is otherwise only
	// reached from dial() — which a pooled request never calls. Reading it
	// here is the whole point: without it the generation could not move in
	// exactly the case this function exists for. A cached hit is one stat.
	if _, err := r.Policy.Current(); err != nil {
		// An unreadable policy is not in force. Retire the pool so the
		// next request must dial, where that error refuses it.
		r.retireTransport()
		return
	}
	gen := r.Policy.Generation()
	// The first observation only records where the policy started; there is
	// nothing pooled under an older one yet.
	if prev := r.policyGen.Swap(gen); prev != 0 && prev != gen {
		r.retireTransport()
	}
}

// retireTransport puts a fresh pool in place and releases what the old
// one still holds idle.
func (r *Relay) retireTransport() {
	if old := r.transport.Swap(r.newTransport()); old != nil {
		old.CloseIdleConnections()
	}
}

// roundTripper returns the transport in force, building the first one on
// demand. A transport per request — the shape this replaces — leaks a
// connection pool and its idle goroutines on every call, which is
// unbounded growth driven by the workload.
func (r *Relay) roundTripper() http.RoundTripper {
	// CompareAndSwap rather than Store: a policy that moved before the
	// first request has already installed one through retireTransport,
	// and storing over it would discard the pool in force.
	r.once.Do(func() {
		r.transport.CompareAndSwap(nil, r.newTransport())
	})
	return r.transport.Load()
}

func (r *Relay) newTransport() *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			host, port, err := parseAuthority(addr)
			if err != nil {
				return nil, err
			}
			return r.dial(ctx, host, port)
		},
		MaxConnsPerHost:        MaxUpstreamConns,
		MaxIdleConns:           MaxUpstreamConns,
		IdleConnTimeout:        90 * time.Second,
		TLSHandshakeTimeout:    15 * time.Second,
		ExpectContinueTimeout:  5 * time.Second,
		ResponseHeaderTimeout:  60 * time.Second,
		MaxResponseHeaderBytes: MaxHeaderBytes,
	}
}

func stripHopByHop(h http.Header) {
	// Headers named by Connection are hop-by-hop for this message,
	// which is the mechanism a smuggling attempt uses to hide one.
	for _, name := range h.Values("Connection") {
		for _, part := range strings.Split(name, ",") {
			if named := strings.TrimSpace(part); named != "" {
				h.Del(named)
			}
		}
	}
	for _, name := range hopByHop {
		h.Del(name)
	}
}

// parseAuthority accepts exactly host:port with a numeric port in
// range, and refuses anything else — empty hosts, missing ports,
// userinfo, paths, control characters or a second colon outside
// brackets. Strictness here is the point: this string decides what the
// gateway connects to.
func parseAuthority(authority string) (host string, port int, err error) {
	if authority == "" {
		return "", 0, errors.New("no destination")
	}
	if len(authority) > 300 {
		return "", 0, errors.New("destination is implausibly long")
	}
	if strings.ContainsAny(authority, " \t\r\n\x00/@?#") {
		return "", 0, errors.New("destination contains invalid characters")
	}
	h, p, err := net.SplitHostPort(authority)
	if err != nil {
		return "", 0, errors.New("destination must be host:port")
	}
	if h == "" {
		return "", 0, errors.New("destination has no host")
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 {
		return "", 0, errors.New("destination has no valid port")
	}
	// An IPv6 literal would have survived SplitHostPort; the sandbox
	// denies IPv6 outright, so refuse it here with a reason instead of
	// failing later at dial.
	if addr, perr := netip.ParseAddr(h); perr == nil && !addr.Unmap().Is4() {
		return "", 0, errors.New("IPv6 destinations are not supported")
	}
	return h, n, nil
}

func defaultPort(hostport, scheme string) string {
	if _, _, err := net.SplitHostPort(hostport); err == nil {
		return hostport
	}
	if scheme == "https" {
		return net.JoinHostPort(hostport, "443")
	}
	return net.JoinHostPort(hostport, "80")
}

// ErrDenied is the policy refusing a destination.
var ErrDenied = errors.New("destination denied by the capsule egress policy")

// dial resolves the target and connects to the first address the policy
// allows. Resolving here, in the gateway, is what makes the check
// meaningful: the capsule hands over a name, never an address, and the
// connection goes to an address validated after resolution.
func (r *Relay) dial(ctx context.Context, host string, port int) (net.Conn, error) {
	// The port set is checked here, not in the callers, because both
	// CONNECT and absolute-URI requests reach the network through this one
	// function. Guarding only the tunnel would leave `GET http://host:9200/`
	// as an unpoliced path to the same destination and port.
	if _, ok := AllowedConnectPorts[port]; !ok {
		r.Log.Warn("egress refused: port outside the allowed set", "host", host, "port", port)
		return nil, fmt.Errorf("%w: port %d is not allowed", ErrDenied, port)
	}
	policy, err := r.Policy.Current()
	if err != nil {
		// A policy that cannot be read is a policy that is not in
		// force: refuse rather than fall back to anything.
		return nil, fmt.Errorf("%w: policy unavailable: %v", ErrDenied, err)
	}
	resolveCtx, cancel := context.WithTimeout(ctx, DNSTimeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupNetIP(resolveCtx, "ip4", host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(addrs) > MaxResolvedAddresses {
		addrs = addrs[:MaxResolvedAddresses]
	}
	var lastErr error
	for _, addr := range addrs {
		addr = addr.Unmap()
		if !policy.Allowed(addr) {
			r.Log.Warn("egress denied", "host", host, "address", addr.String(), "port", port)
			lastErr = fmt.Errorf("%w: %s resolves to %s", ErrDenied, host, addr)
			continue
		}
		dialer := net.Dialer{Timeout: DialTimeout}
		conn, err := dialer.DialContext(ctx, "tcp4", net.JoinHostPort(addr.String(), strconv.Itoa(port)))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no address for %s", host)
	}
	return nil, lastErr
}

// refuse answers a denied or failed destination. A denial is 403 and
// says so: a job that hits the policy should find the reason in its own
// log, not a timeout.
func (r *Relay) refuse(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrDenied) {
		http.Error(w, "runpool egress relay: "+err.Error(), http.StatusForbidden)
		return
	}
	http.Error(w, "runpool egress relay: "+err.Error(), http.StatusBadGateway)
}

// limitListener caps concurrent connections. net.Listener has no such
// bound, and the standard library's Server accepts until the process
// runs out of descriptors.
type limitListener struct {
	net.Listener
	sem chan struct{}
}

// Accept loops rather than recursing on a refusal. Go does not eliminate
// the tail call, so `return l.Accept()` grew one stack frame per refused
// connection and only unwound when one was finally admitted — unbounded
// growth driven by workload input, inside a 128 MiB gateway.
func (l *limitListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.sem <- struct{}{}:
			return &limitConn{Conn: conn, release: func() { <-l.sem }}, nil
		default:
			// Over quota: close immediately. The client sees a closed
			// connection, which is the honest answer — the gateway is at
			// its bound, and holding the connection open would consume the
			// very resource the bound protects.
			conn.Close()
		}
	}
}

type limitConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitConn) Close() error {
	c.once.Do(c.release)
	return c.Conn.Close()
}

// CloseWrite forwards a half-close to the connection underneath.
// net.Conn does not declare it, so embedding the interface hides the
// method even though every connection this wraps has it — and a tunnel
// propagating a half-close would find no closeWriter, close the whole
// connection instead, and truncate the reply it was making room for.
func (c *limitConn) CloseWrite() error {
	cw, ok := c.Conn.(closeWriter)
	if !ok {
		return errors.New("the underlying connection cannot be half-closed")
	}
	return cw.CloseWrite()
}
