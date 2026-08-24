package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
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

	pool atomic.Pointer[pool]

	// pollInterval overrides how often a transfer already in flight
	// re-checks its destination — a tunnel and a plain HTTP transfer
	// alike, so the two shapes cannot drift apart. Zero means
	// tunnelPollInterval, which is what production runs; a test sets it
	// so a revocation can be observed without waiting out the real
	// interval.
	pollInterval time.Duration

	// resolve and dialAddr are how the one property this relay exists for
	// can be asserted: that the address a policy was checked against is
	// the address the connection is made to. Zero means the host resolver
	// and a real dialer, which is what production runs.
	//
	// They are two fields because the property has two halves. A name
	// that answers differently the second time it is asked is the attack
	// -- so a test has to control what a name resolves to, and observe
	// which of those answers was dialed. Neither half is decidable
	// against a real resolver, and the alternative is a control the live
	// suite cannot see either: its own rebinding case uses literal
	// addresses, which prove what the literal-address tests already do.
	resolve  func(ctx context.Context, host string) ([]netip.Addr, error)
	dialAddr func(ctx context.Context, addr netip.Addr, port int) (net.Conn, error)
}

func (r *Relay) poll() time.Duration {
	if r.pollInterval > 0 {
		return r.pollInterval
	}
	return tunnelPollInterval
}

// pool is a connection pool together with the policy generation its
// connections were authorised under.
//
// The two are one value because they are read as one. Held in separate
// atomics, a caller that observed the new generation could still be
// handed the pool that generation retired — it reads them in two steps,
// and the install can land in between — so its request would travel on a
// connection dialled under the policy that no longer applies, which the
// kernel then accepts as established traffic.
type pool struct {
	gen       uint64
	transport *http.Transport
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
	tunnelWith(client, upstream, TunnelIdleTimeout, r.poll(), r.tunnelDestinationAllowed(upstream))
}

// tunnelDestinationAllowed builds the check a running tunnel repeats
// against the policy in force.
func (r *Relay) tunnelDestinationAllowed(upstream net.Conn) func() bool {
	remote := upstream.RemoteAddr().String()
	return func() bool { return r.destinationAllowed(remote) }
}

// destinationAllowed reports whether the policy in force still allows a
// destination that a transfer is already joined to. It reads the peer
// address off the socket rather than the name that was resolved: a name
// can be made to resolve somewhere else between the dial and the
// re-check, and the address is what the policy decides about anyway.
//
// Both shapes the relay serves outlive the decision that authorised
// them, so both repeat this one: a tunnel per poll interval, and a plain
// HTTP transfer on the same period through stopOnRevocation.
//
// Anything it cannot answer is a denial. A policy that cannot be read is
// not in force — the same rule dial applies — and an address that cannot
// be parsed cannot be checked at all.
func (r *Relay) destinationAllowed(remote string) bool {
	peer, err := netip.ParseAddrPort(remote)
	if err != nil {
		r.Log.Warn("egress transfer stopped: peer address unreadable",
			"peer", remote, "error", err)
		return false
	}
	addr := peer.Addr().Unmap()
	policy, err := r.Policy.Current()
	if err != nil {
		r.Log.Warn("egress transfer stopped: policy unavailable",
			"address", addr.String(), "error", err)
		return false
	}
	if policy.Allowed(addr) {
		return true
	}
	r.Log.Warn("egress transfer stopped: destination no longer allowed", "address", addr.String())
	return false
}

// closeWriter is the half-close a tunnel propagates. Every connection a
// tunnel joins has it; naming the interface is what lets a wrapper that
// hides the method be given one back rather than silently falling
// through to a full close.
type closeWriter interface{ CloseWrite() error }

// tunnelPollInterval is how often a direction re-checks the tunnel's
// shared activity clock and the policy its destination was authorised
// under. It bounds two things: how far past the idle timeout a genuinely
// silent tunnel can live, and — the one that matters for egress — how
// long a destination the policy has stopped allowing keeps flowing
// through a tunnel that is already open.
const tunnelPollInterval = 30 * time.Second

// tunnelClock is the activity clock two directions of one tunnel share:
// traffic either way is what keeps the other alive.
//
// Every value it holds is a duration since the tunnel began, never a
// wall-clock instant. The deadlines it is compared against are monotonic
// — that is what net.Conn deadlines and time.Since are — and mixing the
// two means an NTP correction or a VM resume either severs a tunnel that
// is actively streaming or leaves a dead one holding its slot past the
// idle bound.
type tunnelClock struct {
	start time.Time
	last  atomic.Int64
}

func newTunnelClock() *tunnelClock { return &tunnelClock{start: time.Now()} }

// mark records traffic. idleFor reports how long ago the last traffic
// was, in the same monotonic frame.
func (c *tunnelClock) mark()                     { c.last.Store(int64(time.Since(c.start))) }
func (c *tunnelClock) sinceStart() time.Duration { return time.Since(c.start) }
func (c *tunnelClock) idleFor() time.Duration    { return c.sinceStart() - time.Duration(c.last.Load()) }

// tunnelWith copies both directions and returns when both of them have
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
//
// stillAllowed is consulted once per poll interval by each direction:
// a tunnel is authorised at CONNECT and then runs for as long as both
// ends keep talking, so without it a policy tightened underneath one
// applies only to destinations nobody had reached yet.
func tunnelWith(client, upstream net.Conn, idle, poll time.Duration, stillAllowed func() bool) {
	clock := newTunnelClock()

	var wg sync.WaitGroup
	copyIdle := func(dst, src net.Conn) {
		defer wg.Done()
		buf := make([]byte, 32<<10)
		nextCheck := poll
		for {
			// A tunnel outlives the decision that authorised it, so the
			// destination is re-checked while it runs. The bound is
			// elapsed time, not the read deadline below: a direction
			// carrying traffic returns from every read before that
			// deadline fires, and a bulk transfer to a destination just
			// revoked is exactly the case this exists to stop.
			if elapsed := clock.sinceStart(); elapsed >= nextCheck {
				nextCheck = elapsed + poll
				if !stillAllowed() {
					break
				}
			}
			_ = src.SetReadDeadline(time.Now().Add(poll))
			n, err := src.Read(buf)
			if n > 0 {
				clock.mark()
				_ = dst.SetWriteDeadline(time.Now().Add(idle))
				if _, werr := dst.Write(buf[:n]); werr != nil {
					break
				}
				clock.mark()
			}
			switch {
			case err == nil:
				continue
			case errors.Is(err, os.ErrDeadlineExceeded):
				// Quiet here. The tunnel is idle only if it is quiet
				// both ways, which is what the shared clock answers.
				if clock.idleFor() < idle {
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
		// This is an abnormal end — a write that failed, a read error,
		// an idle bound reached, a destination revoked — not the clean
		// half-close above, which returns before here. Close both.
		//
		// Waking the peer with a deadline does not end it: the peer
		// re-arms its own on the next turn of its loop and parks again,
		// and repeats that until the idle bound. That is ten minutes of
		// a connection slot held by a tunnel whose other end has already
		// gone. A close returns the peer's reads immediately and
		// permanently, and the deferred closes further up are the same
		// two calls, only later — this brings them forward to the moment
		// the tunnel actually ended.
		//
		// Closing a socket with unread inbound data sends a reset rather
		// than a clean shutdown, so a revoked destination reaches its
		// client as a reset connection. That is the intent: the transfer
		// is being stopped, not allowed to drain.
		_ = src.Close()
		_ = dst.Close()
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
	outbound, stop := r.tracedRequest(req)
	defer stop()
	outbound.RequestURI = ""
	stripHopByHop(outbound.Header)

	resp, err := r.transportInForce().transport.RoundTrip(outbound)
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
	if _, err := io.Copy(w, resp.Body); err != nil {
		// A body that stops partway must not reach the capsule looking
		// whole. The status line and headers are already out, so there is
		// no way left to say "this failed" except in how the connection
		// ends — and a response with no Content-Length ends when the
		// chunk stream is terminated, which is what returning normally
		// from here would make the server do. The job would then see a
		// well-formed 200 carrying a truncated artifact.
		//
		// Aborting the handler is how the standard library says this: the
		// server closes the connection without a terminator, so a
		// chunked reader reports an unexpected EOF and a Content-Length
		// reader reports a short read. It is the same answer the tunnel
		// gives by resetting rather than draining.
		//
		// Only under a server, which is the one thing that recovers it.
		// Driven directly — by a test holding a recorder, say — the panic
		// would take the process down instead of ending one response.
		if req.Context().Value(http.ServerContextKey) != nil {
			panic(http.ErrAbortHandler)
		}
	}
}

// tracedRequest clones a request onto a context that is cancelled as
// soon as the destination it actually reaches stops being allowed.
//
// The trace is what makes the address knowable. The dial happens inside
// the transport, so nothing here sees the connection until the transport
// reports it, and the name the client asked for is not what the policy
// decides about: it can be made to resolve elsewhere between the dial
// and the re-check.
//
// The watch starts before the round trip rather than after it. A request
// body the client streams is written during the round trip, so a body
// that never ends is a round trip that never returns, and a watch hung
// off its result would never start.
func (r *Relay) tracedRequest(req *http.Request) (*http.Request, context.CancelFunc) {
	ctx, stop := context.WithCancel(req.Context())
	var peer atomic.Pointer[string]
	traced := httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			remote := info.Conn.RemoteAddr().String()
			peer.Store(&remote)
		},
	})
	go r.stopOnRevocation(ctx, stop, &peer)
	return req.Clone(traced), stop
}

// stopOnRevocation cancels a transfer whose destination has stopped
// being allowed.
//
// It is what the poll interval does for a tunnel, applied to the plain
// HTTP shape. A request is authorised once, at dial, and http.Transport
// then streams the request body and the response with no further
// reference to the policy. Retiring the pool does not reach a transfer
// already under way: CloseIdleConnections leaves a connection carrying a
// request alone by contract, and a job can carry one for as long as it
// likes. Neither does the kernel, whose ruleset accepts established
// traffic ahead of every reject. Without this, a destination revoked
// mid-download kept flowing until the job chose to stop.
//
// Cancelling the request context is what ends it, and that covers both
// directions at once — the response body, and a request body the client
// is still streaming.
func (r *Relay) stopOnRevocation(ctx context.Context, stop context.CancelFunc, peer *atomic.Pointer[string]) {
	if r.Policy == nil {
		return
	}
	tick := time.NewTicker(r.poll())
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			// No connection yet is nothing to check: the dial has not
			// happened, so no traffic is flowing to anywhere.
			if remote := peer.Load(); remote != nil && !r.destinationAllowed(*remote) {
				stop()
				return
			}
		}
	}
}

// transportInForce returns the pool authorised by the policy in force,
// replacing it when the policy has moved.
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
// Replacing the pool is what makes the old one unreachable: nothing
// routes through it again, whatever state its connections were in. The
// ones already carrying a request are ended by stopOnRevocation.
func (r *Relay) transportInForce() *pool {
	if r.Policy == nil {
		return r.poolAt(0)
	}
	// Current() is what notices the document changed, and it is otherwise
	// only reached from dial() — which a pooled request never calls.
	// Reading it here is the whole point: without it the generation could
	// not move in exactly the case this function exists for.
	if _, err := r.Policy.Current(); err != nil {
		// An unreadable policy is not in force, and generation zero is a
		// pool nothing was authorised under: the next request has to dial,
		// where that same error refuses it.
		return r.poolAt(0)
	}
	return r.poolAt(r.Policy.Generation())
}

// poolAt returns the pool for a generation, installing a fresh one when
// what is in force belongs to another. A transport per request — the
// shape this replaces — leaks a connection pool and its idle goroutines
// on every call, which is unbounded growth driven by the workload.
func (r *Relay) poolAt(gen uint64) *pool {
	for {
		cur := r.pool.Load()
		// Equal is the ordinary case. Greater means this caller read the
		// policy late — generations only ever advance — so what is in
		// force already supersedes the pool it would install, and
		// installing it would retire a newer pool for nothing and hand
		// the next caller an older tag than the one it observed.
		// Generation zero is the document being unreadable, which is
		// ordered against nothing: it replaces, and is replaced, on any
		// difference.
		if cur != nil && (cur.gen == gen || (gen > 0 && cur.gen > gen)) {
			return cur
		}
		next := &pool{gen: gen, transport: r.newTransport()}
		if r.pool.CompareAndSwap(cur, next) {
			if cur != nil {
				cur.transport.CloseIdleConnections()
			}
			return next
		}
		// Another caller installed one first. Ours never carried a
		// connection; take the winner's on the next turn.
		next.transport.CloseIdleConnections()
	}
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
	addrs, err := r.resolver()(resolveCtx, host)
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
		conn, err := r.dialer()(ctx, addr, port)
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

func (r *Relay) resolver() func(context.Context, string) ([]netip.Addr, error) {
	if r.resolve != nil {
		return r.resolve
	}
	return func(ctx context.Context, host string) ([]netip.Addr, error) {
		return net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
	}
}

func (r *Relay) dialer() func(context.Context, netip.Addr, int) (net.Conn, error) {
	if r.dialAddr != nil {
		return r.dialAddr
	}
	// The validated address, never the name: re-resolving here is what
	// rebinding needs, and there is nothing to re-resolve.
	return func(ctx context.Context, addr netip.Addr, port int) (net.Conn, error) {
		d := net.Dialer{Timeout: DialTimeout}
		return d.DialContext(ctx, "tcp4", net.JoinHostPort(addr.String(), strconv.Itoa(port)))
	}
}
