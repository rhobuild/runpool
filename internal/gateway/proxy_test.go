package gateway

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rhobuild/runpool/internal/atomicfile"
)

// TestParseAuthority is the strictest surface in the gateway: this
// string decides what the process connects to on a workload's behalf.
// Everything that is not exactly host:port must be refused, because a
// looser parser is how a proxy is talked into reaching an address its
// policy never evaluated.
func TestParseAuthority(t *testing.T) {
	valid := map[string]struct {
		host string
		port int
	}{
		"example.com:443":     {"example.com", 443},
		"1.1.1.1:80":          {"1.1.1.1", 80},
		"sub.domain.test:993": {"sub.domain.test", 993},
	}
	for in, want := range valid {
		host, port, err := parseAuthority(in)
		if err != nil || host != want.host || port != want.port {
			t.Errorf("parseAuthority(%q) = %q, %d, %v; want %q, %d", in, host, port, err, want.host, want.port)
		}
	}

	invalid := []string{
		"",
		"example.com",             // no port
		"example.com:",            // empty port
		":443",                    // no host
		"example.com:0",           // port out of range
		"example.com:65536",       // port out of range
		"example.com:https",       // non-numeric port
		"example.com:443/path",    // a path is not part of an authority
		"user@example.com:443",    // userinfo
		"example.com:443?q=1",     // query
		"example.com:443#f",       // fragment
		"exa mple.com:443",        // space
		"example.com:443\r\nX: y", // header injection
		"example.com:443\x00",     // NUL
		"[2606:4700::1111]:443",   // IPv6 has no path out of the sandbox
		strings.Repeat("a", 400) + ":443",
	}
	for _, in := range invalid {
		if host, port, err := parseAuthority(in); err == nil {
			t.Errorf("parseAuthority(%q) accepted %q:%d; want an error", in, host, port)
		}
	}
}

// FuzzParseAuthority: whatever it accepts must be a host and a port in
// range, and must round-trip through net.JoinHostPort unchanged. A
// parser that accepts something it cannot reconstruct is a parser two
// components will disagree about.
func FuzzParseAuthority(f *testing.F) {
	for _, seed := range []string{
		"example.com:443", "1.1.1.1:80", "", ":", "a:b", "[::1]:53",
		"host:443/x", "h:99999", "h:443\r\n",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		host, port, err := parseAuthority(in)
		if err != nil {
			return
		}
		if host == "" {
			t.Fatalf("accepted %q with an empty host", in)
		}
		if port < 1 || port > 65535 {
			t.Fatalf("accepted %q with port %d", in, port)
		}
		if strings.ContainsAny(host, " \t\r\n\x00/@?#") {
			t.Fatalf("accepted %q with a hostile host %q", in, host)
		}
	})
}

// TestStripHopByHop: hop-by-hop headers describe one connection, and a
// proxy that forwards them invites request smuggling. Headers *named*
// by Connection are hop-by-hop for that message, which is exactly the
// mechanism used to hide one.
func TestStripHopByHop(t *testing.T) {
	h := http.Header{}
	h.Set("Connection", "X-Custom-Hop, Upgrade")
	h.Set("X-Custom-Hop", "smuggled")
	h.Set("Transfer-Encoding", "chunked")
	h.Set("Proxy-Authorization", "secret")
	h.Set("Keep-Alive", "timeout=5")
	h.Set("Upgrade", "websocket")
	h.Set("X-Kept", "yes")

	stripHopByHop(h)

	for _, gone := range []string{
		"Connection", "X-Custom-Hop", "Transfer-Encoding",
		"Proxy-Authorization", "Keep-Alive", "Upgrade",
	} {
		if h.Get(gone) != "" {
			t.Errorf("%s survived the hop-by-hop strip", gone)
		}
	}
	if h.Get("X-Kept") != "yes" {
		t.Error("an end-to-end header was stripped")
	}
}

// TestConnectPortPolicy pins the relay's honest contract: CONNECT is a
// TCP tunnel, so the port set is what keeps it from being arbitrary TCP
// egress. A port outside the set is refused before any dial, with a
// reason a job can read.
func TestConnectPortPolicy(t *testing.T) {
	r := &Relay{Log: discardLogger()}

	for _, port := range []string{"22", "25", "3306", "6379", "9999"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodConnect, "http://example.com", nil)
		req.Host = "example.com:" + port
		r.connect(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("CONNECT to port %s = %d; want 403", port, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "not allowed") {
			t.Errorf("CONNECT to port %s did not say why: %q", port, rec.Body.String())
		}
	}
	for _, port := range []int{80, 443} {
		if _, ok := AllowedConnectPorts()[port]; !ok {
			t.Errorf("port %d must be in the allowed set; CI cannot work without it", port)
		}
	}
}

// TestMalformedRequests: a request that is not a proxy request gets a
// 400, not a panic and not a connection attempt.
func TestMalformedRequests(t *testing.T) {
	r := &Relay{Log: discardLogger()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/relative/path", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("relative-path request = %d; want 400", rec.Code)
	}

	rec = httptest.NewRecorder()
	bad := httptest.NewRequest(http.MethodConnect, "http://example.com", nil)
	bad.Host = "not an authority"
	r.connect(rec, bad)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("malformed CONNECT authority = %d; want 400", rec.Code)
	}
}

// TestDeniedWithoutPolicy: a relay whose policy cannot be read refuses
// every destination. A gateway with no policy must deny, not allow.
func TestDeniedWithoutPolicy(t *testing.T) {
	r := &Relay{Policy: &PolicyStore{Path: t.TempDir() + "/absent.json"}, Log: discardLogger()}
	if _, err := r.dial(t.Context(), "example.com", 443); err == nil {
		t.Fatal("dial succeeded with no policy in force")
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestForwardAppliesThePortPolicy: the plain-HTTP path reaches the network
// through the same dialer as CONNECT, so the port set has to bind there too.
// Guarding only the tunnel left `GET http://host:9200/` as an unpoliced route
// to the very destination and port CONNECT refuses.
func TestForwardAppliesThePortPolicy(t *testing.T) {
	r := &Relay{Log: discardLogger()}

	for _, target := range []string{
		"http://example.com:9200/_cluster/state",
		"http://example.com:6379/",
		"http://example.com:2375/containers/json",
	} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusForbidden {
			t.Errorf("plain HTTP to %s = %d; want 403", target, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "not allowed") {
			t.Errorf("%s refusal did not say why: %q", target, rec.Body.String())
		}
	}
}

// hijackedClient is a ResponseWriter that hands back a connection plus a
// reader which already holds bytes — the state net/http leaves behind
// when a client pipelines its first payload into the same segment as the
// CONNECT header block. The stdlib documents it plainly: "the returned
// bufio.Reader may contain unprocessed buffered data from the client".
type hijackedClient struct {
	conn     net.Conn
	buffered *bufio.ReadWriter
}

func (h *hijackedClient) Header() http.Header         { return http.Header{} }
func (h *hijackedClient) Write(b []byte) (int, error) { return len(b), nil }
func (h *hijackedClient) WriteHeader(int)             {}
func (h *hijackedClient) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return h.conn, h.buffered, nil
}

// TestTunnelCarriesTheBytesTheServerAlreadyRead. A client may send its
// first payload without waiting for the 200 — pipelining after CONNECT
// is legal, and the dial that precedes the hijack gives it up to a
// minute of opportunity. Those bytes are out of the socket and inside
// the server's own reader by then, so a tunnel that copies from the raw
// connection loses them: upstream never sees the ClientHello, the client
// waits for a ServerHello, and neither side errors. It hangs until the
// idle timeout, holding one of the relay's connection slots.
func TestTunnelCarriesTheBytesTheServerAlreadyRead(t *testing.T) {
	pipelined := []byte("\x16\x03\x01\x00\x2fClientHello-pipelined")
	afterwards := []byte("...and what the client sends next")

	// The upstream records the stream in the order it arrives.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan []byte, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, len(pipelined)+len(afterwards))
		n, _ := io.ReadFull(conn, buf)
		got <- buf[:n]
	}()
	upstream, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()

	// The client's end, with the pipelined bytes already buffered ahead
	// of the live connection — exactly how net/http hands them over.
	clientSide, serverSide := net.Pipe()
	defer clientSide.Close()
	reader := bufio.NewReader(io.MultiReader(bytes.NewReader(pipelined), serverSide))
	if _, err := reader.Peek(1); err != nil {
		t.Fatal(err)
	}
	if reader.Buffered() != len(pipelined) {
		t.Fatalf("fixture buffered %d bytes; want the whole pipelined payload, %d",
			reader.Buffered(), len(pipelined))
	}
	w := &hijackedClient{
		conn:     serverSide,
		buffered: bufio.NewReadWriter(reader, bufio.NewWriter(serverSide)),
	}

	// A real policy store, not a nil one: establishTunnel now starts a
	// tunnel that consults the policy on its own schedule, and a fixture
	// that passes only because the poll interval outlasts the test is a
	// fixture that stops passing the day someone shortens it.
	policyDir := t.TempDir()
	if err := os.WriteFile(PolicyPath(policyDir),
		[]byte(`{"internal_subnet":"172.31.0.0/24","uplink_subnet":"172.31.1.0/24",`+
			`"allow":["0.0.0.0/0"],"deny":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &Relay{Policy: &PolicyStore{Path: PolicyPath(policyDir)}, Log: discardLogger()}
	done := make(chan struct{})
	go func() { defer close(done); r.establishTunnel(w, upstream) }()

	// Drain the 200 the relay writes, then send the rest.
	if _, err := bufio.NewReader(clientSide).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if _, err := clientSide.Write(afterwards); err != nil {
		t.Fatal(err)
	}

	select {
	case stream := <-got:
		want := append(append([]byte{}, pipelined...), afterwards...)
		if !bytes.Equal(stream, want) {
			t.Errorf("upstream received %q;\nwant the pipelined payload first, then the rest: %q",
				stream, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never received the full stream; the pipelined bytes were dropped")
	}
	clientSide.Close()
	<-done
}

// alwaysAllowed is the destination check for tunnels whose policy is not
// what the test is about.
func alwaysAllowed() bool { return true }

// tcpPair dials a loopback listener and hands back both ends of one real
// TCP connection. Real sockets, not net.Pipe: a half-close is a property
// of the transport, and a pipe has no such thing.
func tcpPair(t *testing.T) (client, server net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		accepted <- c
	}()
	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case server = <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("the listener never accepted")
	}
	t.Cleanup(func() { client.Close(); server.Close() })
	return client, server
}

// TestAHalfClosingClientStillReceivesItsWholeResponse: a client that
// shuts down its write side once its request is out must still get the
// whole reply.
//
// It is ordinary HTTP client behaviour and a clean EOF on one direction
// only, not the end of the connection. Treating it as the end of the
// tunnel closes the socket the upstream is still answering on, and the
// client gets a truncated response with nothing to say why.
func TestAHalfClosingClientStillReceivesItsWholeResponse(t *testing.T) {
	const size = 1 << 20
	clientEnd, clientSide := tcpPair(t)
	upstreamSide, upstreamEnd := tcpPair(t)

	go tunnelWith(clientSide, upstreamSide, 5*time.Second, 500*time.Millisecond, alwaysAllowed)

	// The request goes out, then the client is done sending.
	if _, err := clientEnd.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	if err := clientEnd.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}

	// The upstream sees the request and its EOF, then answers at length.
	go func() {
		buf := make([]byte, 64)
		for {
			if _, err := upstreamEnd.Read(buf); err != nil {
				break
			}
		}
		payload := bytes.Repeat([]byte("x"), size)
		_, _ = upstreamEnd.Write(payload)
		_ = upstreamEnd.(*net.TCPConn).CloseWrite()
	}()

	if err := clientEnd.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(clientEnd)
	if err != nil {
		t.Fatalf("reading the response: %v (got %d of %d bytes)", err, len(got), size)
	}
	if len(got) != size {
		t.Errorf("the client received %d bytes of a %d-byte response; the half-close was "+
			"read as the end of the tunnel", len(got), size)
	}
}

// TestALimitedConnectionForwardsAHalfClose: the wrapper the relay counts
// connections with must not hide the half-close.
//
// It embeds net.Conn, which does not declare CloseWrite, so the method
// is not promoted — a tunnel looking for one finds nothing and closes
// the whole connection instead, which is the truncation above by another
// route.
func TestALimitedConnectionForwardsAHalfClose(t *testing.T) {
	peer, raw := tcpPair(t)
	limited := &limitConn{Conn: raw, release: func() {}}

	var cw closeWriter = limited // fails to compile if the method is gone
	if err := cw.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}

	// The peer sees EOF on its read side...
	if err := peer.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(peer); err != nil {
		t.Fatalf("the peer could not read to EOF after the half-close: %v", err)
	}
	// ...and the half-closed side can still read what the peer sends.
	if _, err := peer.Write([]byte("reply")); err != nil {
		t.Fatal(err)
	}
	if err := limited.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(limited, buf); err != nil {
		t.Fatalf("the half-closed connection lost its read side: %v", err)
	}
}

// TestAOneWayTransferOutlivesTheIdleBound: a transfer that is silent in
// one direction is not an idle tunnel.
//
// A large upload or a slow download says nothing back for its whole
// duration, so a deadline applied per direction severed connections
// that were actively streaming. The bound belongs to the tunnel, and
// the tunnel is idle only when neither direction has moved.
func TestAOneWayTransferOutlivesTheIdleBound(t *testing.T) {
	const (
		idle  = 200 * time.Millisecond
		poll  = 20 * time.Millisecond
		beats = 40
	)
	clientEnd, clientSide := tcpPair(t)
	upstreamSide, upstreamEnd := tcpPair(t)

	go tunnelWith(clientSide, upstreamSide, idle, poll, alwaysAllowed)

	// One direction streams steadily for well past the idle bound; the
	// other says nothing at all.
	go func() {
		for range beats {
			if _, err := upstreamEnd.Write([]byte("x")); err != nil {
				return
			}
			time.Sleep(30 * time.Millisecond)
		}
		_ = upstreamEnd.(*net.TCPConn).CloseWrite()
	}()

	if err := clientEnd.SetReadDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(clientEnd)
	if err != nil {
		t.Fatalf("reading the stream: %v (got %d of %d bytes)", err, len(got), beats)
	}
	if len(got) != beats {
		t.Errorf("the client received %d of %d bytes over %s with a %s idle bound; "+
			"the quiet direction expired while the other was streaming",
			len(got), beats, time.Duration(beats)*30*time.Millisecond, idle)
	}
}

// TestADeadTunnelReleasesItsSlotAtOnce: a tunnel that has ended gives up
// its connection slot when it ends, not when the idle bound expires.
//
// Ending it by nudging the peer's read deadline does not end it. The
// peer wakes, finds the shared clock recent, and re-arms its own
// deadline on the next turn of its loop, and it repeats that until the
// idle bound — ten minutes during which the relay's connection quota is
// one smaller for a tunnel with nothing on either end.
func TestADeadTunnelReleasesItsSlotAtOnce(t *testing.T) {
	const (
		idle = 3 * time.Second
		poll = 100 * time.Millisecond
	)
	clientEnd, clientSide := tcpPair(t)
	upstreamSide, upstreamEnd := tcpPair(t)
	_ = clientEnd

	done := make(chan struct{})
	go func() {
		tunnelWith(clientSide, upstreamSide, idle, poll, alwaysAllowed)
		close(done)
	}()

	// A reset, not a clean shutdown: the upstream is gone and says so
	// with an error, which is the abnormal end this is about.
	if err := upstreamEnd.(*net.TCPConn).SetLinger(0); err != nil {
		t.Fatal(err)
	}
	if err := upstreamEnd.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case <-done:
	case <-time.After(idle - time.Second):
		t.Fatalf("the tunnel outlived its upstream by more than %v; it is waiting out the idle bound", idle-time.Second)
	}
}

// TestARevokedDestinationEndsALiveTunnel: a destination the policy stops
// allowing stops flowing, within one poll interval, while it is still
// carrying traffic.
//
// A tunnel is authorised once, at CONNECT, and then runs for as long as
// both ends keep talking. A policy tightened underneath it — a lease
// narrowing its allowances, a redelivery installing a smaller set — has
// to reach the connections already open, or the tightening only applies
// to destinations nobody had reached yet.
//
// The traffic is the point: this stays busy throughout, and a direction
// carrying bytes returns from every read before its deadline fires, so a
// check hung off that deadline would never run here.
func TestARevokedDestinationEndsALiveTunnel(t *testing.T) {
	const (
		idle = 30 * time.Second
		poll = 50 * time.Millisecond
	)
	clientEnd, clientSide := tcpPair(t)
	upstreamSide, upstreamEnd := tcpPair(t)

	var allowed atomic.Bool
	allowed.Store(true)

	done := make(chan struct{})
	go func() {
		tunnelWith(clientSide, upstreamSide, idle, poll, allowed.Load)
		close(done)
	}()

	// Both ends write, and both ends are drained. One-way traffic would
	// not do: the silent direction's read deadline fires every poll, so a
	// check hung off that deadline runs there and the test would pass
	// against exactly the shape it exists to rule out.
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	stream := func(w net.Conn) {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := w.Write([]byte("x")); err != nil {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
	drain := func(rd net.Conn) {
		buf := make([]byte, 4096)
		for {
			if _, err := rd.Read(buf); err != nil {
				return
			}
		}
	}
	go stream(upstreamEnd)
	go stream(clientEnd)

	// Do not revoke anything until both directions are demonstrably
	// carrying traffic, or the test could pass against a tunnel that
	// never ran, or one running one-way.
	for _, end := range []net.Conn{clientEnd, upstreamEnd} {
		if err := end.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(end, make([]byte, 8)); err != nil {
			t.Fatalf("the tunnel never carried anything towards %s: %v", end.LocalAddr(), err)
		}
		if err := end.SetReadDeadline(time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	go drain(clientEnd)
	go drain(upstreamEnd)

	allowed.Store(false)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the tunnel kept flowing to a destination the policy no longer allows")
	}
}

// TestTheTunnelClockIsMonotonic: the clock two directions share measures
// in a frame no clock correction can move.
//
// Every deadline around it is monotonic, because that is what net.Conn
// deadlines and time.Since are. A wall-clock instant mixed in means an
// NTP step or a VM resume either severs a tunnel mid-transfer or leaves
// a finished one holding its slot long past the idle bound, and neither
// shows up as anything but an unexplained connection.
func TestTheTunnelClockIsMonotonic(t *testing.T) {
	c := newTunnelClock()

	// Round(0) is what strips a monotonic reading, so a time that still
	// carries one is not equal to itself rounded.
	if c.start == c.start.Round(0) {
		t.Fatal("the tunnel clock's origin carries no monotonic reading; every bound derived from it moves with the wall clock")
	}

	time.Sleep(20 * time.Millisecond)
	before := c.idleFor()
	c.mark()
	if after := c.idleFor(); after >= before {
		t.Errorf("idleFor = %v after marking traffic and %v before; a mark must reset it", after, before)
	}
}

// dialedTo stands in for the connection an http.Transport reports
// through its trace. Only the remote address is ever read: it is the one
// thing a re-check needs and the one thing a net.Pipe cannot supply.
type dialedTo struct {
	net.Conn
	remote net.Addr
}

func (c dialedTo) RemoteAddr() net.Addr { return c.remote }

// policyFile writes a gateway policy denying one range and returns a
// store reading it, plus the installer for later revisions.
//
// Installs go through atomicfile.Replace because that is what Reload
// does. os.WriteFile truncates before it writes, so a reader crossing
// one sees an empty document, refuses to compile it, and treats the
// policy as unavailable — a denial arrived at through a shape production
// cannot produce, which would let these tests pass for a reason that is
// not the one they name.
func policyFile(t *testing.T, deny string) (*PolicyStore, func(string)) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.json")
	write := func(deny string) {
		body := `{"internal_subnet":"172.31.0.0/24","uplink_subnet":"172.31.1.0/24",` +
			`"allow":[],"deny":["` + deny + `"]}`
		if err := atomicfile.Replace(path, []byte(body), 0o600, -1, -1); err != nil {
			t.Error(err)
		}
	}
	write(deny)
	return &PolicyStore{Path: path}, write
}

// TestARevokedDestinationEndsALiveTransfer: a plain HTTP transfer is
// authorised once, at the dial, and then streams with no further
// reference to the policy.
//
// Nothing else reaches it. Retiring the pool does not: CloseIdleConnections
// leaves a connection carrying a request alone by contract, and a job can
// carry one for as long as it likes. The kernel does not either: the
// ruleset accepts established traffic ahead of every reject. So without
// this re-check, a destination revoked mid-download kept flowing until the
// job chose to stop, which is the promise in Reload's own doc — that a
// capsule never sees a moment in which a newly denied destination is
// reachable — going unkept on the one path that carries bulk traffic.
func TestARevokedDestinationEndsALiveTransfer(t *testing.T) {
	store, install := policyFile(t, "10.0.0.0/8")
	r := &Relay{Policy: store, Log: discardLogger(), pollInterval: 20 * time.Millisecond}

	outbound, stop := r.tracedRequest(httptest.NewRequest(http.MethodGet, "http://example.com/big", nil))
	defer stop()

	// Report the connection the way http.Transport does once it has
	// dialled. Until this lands there is no address to re-check.
	httptrace.ContextClientTrace(outbound.Context()).GotConn(httptrace.GotConnInfo{
		Conn: dialedTo{remote: &net.TCPAddr{IP: net.IPv4(203, 0, 113, 7), Port: 80}},
	})

	// While the destination is allowed the transfer is left alone.
	select {
	case <-outbound.Context().Done():
		t.Fatal("an allowed transfer was cancelled; every plain HTTP download would be cut at the poll interval")
	case <-time.After(200 * time.Millisecond):
	}

	install("203.0.113.0/24")

	select {
	case <-outbound.Context().Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the transfer outlived the revocation of its destination; " +
			"the body keeps streaming from an address the policy now denies")
	}
}

// TestATransferWithoutAConnectionIsNotCancelled: the re-check has
// nothing to decide about until the transport reports a dial, and a
// denial in that gap would refuse requests the policy allows.
func TestATransferWithoutAConnectionIsNotCancelled(t *testing.T) {
	store, _ := policyFile(t, "10.0.0.0/8")
	r := &Relay{Policy: store, Log: discardLogger(), pollInterval: 10 * time.Millisecond}

	outbound, stop := r.tracedRequest(httptest.NewRequest(http.MethodGet, "http://example.com/", nil))
	defer stop()

	select {
	case <-outbound.Context().Done():
		t.Fatal("a request was cancelled before it had a connection; a slow dial would never complete")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestConcurrentReadersNeverSplitThePoolFromItsGeneration: the pool and
// the generation it was authorised under are one value because they are
// read as one.
//
// Held in two atomics, a caller that observed the new generation could
// still be handed the pool that generation retired — it reads them in two
// steps and the install lands in between — so its request would travel on
// a connection dialled under the policy that no longer applies, which the
// kernel then accepts as established traffic.
//
// The document here is only ever replaced, never removed, so every read
// succeeds and the generation only advances. That is what makes one pool
// per generation the right thing to assert: it is a consequence of this
// sequence rather than a property of poolAt, whose full condition —
// including what an unreadable document does to it — is stated case by
// case in TestAPoolIsNeverOlderThanTheGenerationItServes.
func TestConcurrentReadersNeverSplitThePoolFromItsGeneration(t *testing.T) {
	store, install := policyFile(t, "10.0.0.0/8")
	r := &Relay{Policy: store, Log: discardLogger()}

	var mu sync.Mutex
	served := map[uint64]*http.Transport{}

	// installed counts the installer's attempts, not the generations any
	// reader saw: the store advances a generation when a caller observes
	// a changed file, so two writes with no read between them still move
	// it once. That is fine for what this is used for -- it is how a
	// reader knows the installer is running at all, and the count of
	// generations actually observed is asserted separately at the end,
	// unconditionally, which is what makes the answer true rather than
	// this. The
	// readers below wait on it rather than on a fixed number of their
	// own iterations, because a reader here never blocks: it is a map
	// read and an atomic load, so on a host with fewer cores than this
	// test has goroutines the installer can be starved for the whole run
	// and every reader finish having seen one generation. That is what
	// the guard at the end catches, and catching it is the test failing
	// rather than the code.
	var installed atomic.Uint64
	stop := make(chan struct{})
	var installers sync.WaitGroup
	installers.Add(1)
	go func() {
		defer installers.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			install(fmt.Sprintf("10.%d.0.0/16", i%256))
			installed.Add(1)
		}
	}()

	// The budget is the whole test's, not one reader's: it bounds a
	// starved installer into a failure that says so instead of a hang.
	// Named because the message at the end quotes it, and a second
	// literal there would keep saying thirty seconds after someone
	// changed this one.
	const readerBudget = 30 * time.Second
	deadline := time.Now().Add(readerBudget)
	var readers sync.WaitGroup
	for range 8 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			// At least 400 reads, and not finished until the policy has
			// moved twice under this reader. Twice and not once: one
			// install may have landed before the first read, which would
			// leave the reader having seen a single generation again.
			from := installed.Load()
			for reads := 0; reads < 400 || installed.Load() < from+2; reads++ {
				if time.Now().After(deadline) {
					return
				}
				got := r.transportInForce()
				mu.Lock()
				if prev, seen := served[got.gen]; seen && prev != got.transport {
					t.Errorf("generation %d was served by two pools; a request took a connection "+
						"dialled under the policy that generation replaced", got.gen)
				}
				served[got.gen] = got.transport
				mu.Unlock()
				// Nothing in this loop blocks, so nothing yields. On a
				// host with fewer cores than this test has goroutines,
				// that is enough to keep the installer off a processor
				// for the whole run.
				runtime.Gosched()
			}
		}()
	}
	readers.Wait()
	close(stop)
	installers.Wait()

	if len(served) < 2 {
		t.Fatalf("only %d generation(s) were observed in %s; the policy never moved under the "+
			"readers and the race this rules out was never given a chance",
			len(served), readerBudget)
	}
}

// TestARevokedTransferDoesNotReachTheCapsuleLookingWhole drives the real
// handler: a real http.Transport, the trace that reports its dial, the
// watch forward installs, and the copy that has to stop.
//
// The body is chunked deliberately. With no Content-Length, the only
// thing that tells a client where the body ended is how the connection
// ended — so a relay that cancels the transfer and then returns normally
// has the server terminate the chunk stream for it, and the job receives
// a well-formed 200 carrying a truncated artifact it cannot tell from a
// whole one. Git's smart-HTTP pack responses and anything gzipped on the
// fly arrive this way.
//
// The relay's own dialer refuses loopback outright — the decider does so
// before it consults any allow list — so the upstream is reached through
// a plain transport installed at the generation in force. That leaves the
// re-check exactly as production runs it, and it denies loopback on the
// first tick, which is the revocation this test needs.
func TestARevokedTransferDoesNotReachTheCapsuleLookingWhole(t *testing.T) {
	const (
		chunk  = 32 << 10
		chunks = 200
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		body := bytes.Repeat([]byte("x"), chunk)
		for range chunks {
			if _, err := w.Write(body); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			time.Sleep(5 * time.Millisecond)
		}
	}))
	defer upstream.Close()

	store, _ := policyFile(t, "10.0.0.0/8")
	if _, err := store.Current(); err != nil {
		t.Fatal(err)
	}
	r := &Relay{Policy: store, Log: discardLogger(), pollInterval: 20 * time.Millisecond}
	r.pool.Store(&pool{gen: store.Generation(), transport: &http.Transport{}})

	relay := httptest.NewServer(r)
	defer relay.Close()
	via, err := url.Parse(relay.URL)
	if err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(via)}}
	resp, err := client.Get(upstream.URL + "/big")
	if err != nil {
		t.Fatalf("the request never reached the upstream: %v", err)
	}
	defer resp.Body.Close()
	n, readErr := io.Copy(io.Discard, resp.Body)

	// Prove the transfer happened before judging how it ended. A relay
	// that refused the request outright also reports a short body that
	// stopped, and blaming that on the ending would hide the refusal.
	if resp.StatusCode != http.StatusOK || n == 0 {
		t.Fatalf("the request never streamed: status %d, %d bytes; the upstream was not reached, "+
			"so nothing here says anything about how a cut transfer ends", resp.StatusCode, n)
	}
	if n >= int64(chunk)*chunks {
		t.Fatalf("the transfer delivered its whole body (%d bytes); the revocation never reached it", n)
	}
	if readErr == nil {
		t.Errorf("the revoked transfer ended cleanly after %d of %d bytes; the capsule cannot "+
			"tell a cut artifact from a whole one", n, chunk*chunks)
	}
}

// TestAPoolIsNeverOlderThanTheGenerationItServes states what poolAt's
// condition means, case by case, where the concurrent test can only
// sample it.
//
// The property that matters is not that a generation is served by one
// pool for all time — restoring a document a reader had already seen
// leaves the generation where it was while the pool moved through zero,
// and that is harmless because every connection is checked at its dial.
// It is that a caller which observed a generation is never handed a pool
// built for an older one.
func TestAPoolIsNeverOlderThanTheGenerationItServes(t *testing.T) {
	for _, c := range []struct {
		name       string
		pooled     bool // whether anything is in force to begin with
		inForce    uint64
		ask        uint64
		wantReused bool
	}{
		{name: "nothing pooled yet", ask: 5},
		{name: "same generation", pooled: true, inForce: 5, ask: 5, wantReused: true},
		{name: "the policy moved on", pooled: true, inForce: 3, ask: 5},
		{name: "a caller that read the policy late", pooled: true, inForce: 5, ask: 3, wantReused: true},
		{name: "the document became unreadable", pooled: true, inForce: 5, ask: 0},
		{name: "still unreadable", pooled: true, inForce: 0, ask: 0, wantReused: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := &Relay{Log: discardLogger()}
			var before *pool
			if c.pooled {
				before = &pool{gen: c.inForce, transport: r.newTransport()}
				r.pool.Store(before)
			}
			got := r.poolAt(c.ask)
			if got == nil {
				t.Fatal("no pool at all; the next request would have nothing to travel on")
			}
			if reused := before != nil && got == before; reused != c.wantReused {
				t.Errorf("reused the pool in force = %v; want %v", reused, c.wantReused)
			}
			// A reused pool is the only way to be handed a generation
			// other than the one asked for, and it must never be an older
			// one: its connections were authorised by that policy.
			if got.gen < c.ask {
				t.Errorf("a caller that observed generation %d was handed a pool built for %d; "+
					"its connections were authorised by a policy that no longer applies", c.ask, got.gen)
			}
		})
	}
}

// TestTheAddressCheckedIsTheAddressDialed is the DNS rebinding defence,
// and until now nothing held it.
//
// The attack is a name that answers one address while the policy is
// consulted and another when the connection is made. This relay is not
// vulnerable to it because it dials the address it checked rather than
// the name -- but that is a property of one line, and the line could be
// changed back to a name with every test in this package and the live
// bypass suite still passing. That suite calls its own section the
// rebinding case and gives it literal addresses, which prove what the
// literal-address tests already prove.
//
// So the resolver and the dial are both seams here: one to say what the
// name answers, the other to observe which of those answers was used.
func TestTheAddressCheckedIsTheAddressDialed(t *testing.T) {
	metadata := netip.MustParseAddr("169.254.169.254")
	allowed := netip.MustParseAddr("93.184.216.34")

	for name, tc := range map[string]struct {
		answers    []netip.Addr
		wantDial   string // empty means no connection may be made
		wantDenied bool
	}{
		"a name that answers the metadata address is refused": {
			answers: []netip.Addr{metadata}, wantDenied: true,
		},
		"a name that answers an allowed address is dialed at that address": {
			answers: []netip.Addr{allowed}, wantDial: allowed.String(),
		},
		"a denied answer does not poison the allowed one beside it": {
			answers: []netip.Addr{metadata, allowed}, wantDial: allowed.String(),
		},
		"every answer denied refuses rather than falling through": {
			answers: []netip.Addr{metadata, netip.MustParseAddr("10.1.2.3")}, wantDenied: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			store, _ := policyFile(t, "10.0.0.0/8")
			var dialed []string
			r := &Relay{
				Policy: store,
				Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
				resolve: func(context.Context, string) ([]netip.Addr, error) {
					return tc.answers, nil
				},
				dialAddr: func(_ context.Context, addr netip.Addr, _ int) (net.Conn, error) {
					dialed = append(dialed, addr.String())
					client, server := net.Pipe()
					t.Cleanup(func() { client.Close(); server.Close() })
					return client, nil
				},
			}

			conn, err := r.dial(t.Context(), "rebinding.test", 443)
			if conn != nil {
				conn.Close()
			}
			if tc.wantDenied {
				if !errors.Is(err, ErrDenied) {
					t.Fatalf("dial = %v; want ErrDenied", err)
				}
				if len(dialed) != 0 {
					t.Errorf("connected to %v; a denied answer must reach no network at all", dialed)
				}
				return
			}
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			if len(dialed) != 1 || dialed[0] != tc.wantDial {
				t.Errorf("connected to %v; want exactly %q — the address the policy was checked "+
					"against is the address the connection is made to", dialed, tc.wantDial)
			}
		})
	}
}

// TestTheAllowedPortSetIsExactlyTwo: the set is the relay's contract,
// and a sample of it is not the contract.
//
// A CONNECT tunnel carries any protocol a proxy-aware client speaks, so
// the port set is what keeps an allowed address from becoming arbitrary
// TCP to that address. The existing tests prove 80 and 443 are in it and
// that five other ports are not, which a wider map still satisfies: a
// sixth port added here is a widening nothing reads back.
func TestTheAllowedPortSetIsExactlyTwo(t *testing.T) {
	want := map[int]string{80: "HTTP", 443: "HTTPS"}
	got := AllowedConnectPorts()
	if len(got) != len(want) {
		t.Fatalf("the relay allows %d ports: %v. Every one is a protocol a tunnel can carry "+
			"to an allowed address", len(got), got)
	}
	for port, name := range want {
		if label, ok := got[port]; !ok || label != name {
			t.Errorf("port %d is %q, %v; want %q", port, label, ok, name)
		}
	}
	got[22] = "SSH"
	if _, ok := AllowedConnectPorts()[22]; ok {
		t.Error("mutating the returned port policy widened the relay's live policy")
	}
}
