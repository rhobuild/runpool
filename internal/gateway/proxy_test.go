package gateway

import (
	"bufio"
	"bytes"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
		if _, ok := AllowedConnectPorts[port]; !ok {
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

	r := &Relay{Log: discardLogger()}
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

	go tunnelWith(clientSide, upstreamSide, 5*time.Second, 500*time.Millisecond)

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

	go tunnelWith(clientSide, upstreamSide, idle, poll)

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
