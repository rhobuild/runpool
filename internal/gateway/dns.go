package gateway

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"time"
)

// Bounds on the DNS relay.
const (
	// DNSTimeout bounds one upstream query.
	DNSTimeout = 5 * time.Second
	// MaxDNSInFlight bounds concurrent upstream queries. A capsule can
	// emit UDP packets far faster than a resolver answers; without this
	// each one would become a goroutine and a socket.
	MaxDNSInFlight = 64
	// MaxDNSMessage is the largest message the relay will carry over
	// UDP. EDNS(0) advertises up to 4096 in practice, and a datagram is
	// not the transport for anything larger: an answer that does not fit
	// is what TC and the TCP retry exist for.
	MaxDNSMessage = 4096
	// MaxDNSMessageTCP is the largest it will carry over TCP: everything
	// a two-byte length prefix can frame. TCP exists precisely to carry
	// the answers a datagram cannot, so capping it at the UDP size would
	// leave large but ordinary records — a signed zone's chain, a long
	// TXT set — unreachable by any transport through this relay. The
	// memory is still bounded by MaxDNSConns: 32 connections of 64 KiB.
	MaxDNSMessageTCP = 65535
	// MinDNSMessage is a header: anything shorter cannot be a query.
	MinDNSMessage = 12
	// MaxDNSConns bounds concurrent TCP DNS connections.
	MaxDNSConns = 32
	// MaxResolvedAddresses bounds how many addresses the relay will try
	// for one name, so a hostile answer with hundreds of records cannot
	// turn one request into hundreds of dial attempts.
	MaxResolvedAddresses = 8
)

// defaultUpstream is the daemon's embedded resolver, on this
// container's loopback. Docker installs the nat rules that make it
// reachable, which is why the gateway's own ruleset must never flush
// the nat table.
const defaultUpstream = "127.0.0.11:53"

// DNSRelay is the capsule's resolver: a bounded payload relay from the
// internal leg to the daemon's embedded resolver.
//
// It deliberately does not parse names or filter answers. A name is not
// a destination — the address policy is applied at connect time, where
// an answer cannot be swapped afterwards — so inspecting answers here
// would add a parser without adding a defence. What it does enforce is
// shape: a message must be a plausible DNS message, of bounded size,
// and its answer must carry the same transaction ID as the query.
type DNSRelay struct {
	Log *slog.Logger
	// Upstream is the resolver every query is relayed to. Empty means
	// the daemon's embedded one, which is what production runs; a test
	// points it at a resolver it controls, because the answers worth
	// testing are the ones a real resolver cannot be asked to give.
	Upstream string
}

func (d *DNSRelay) resolver() string {
	if d.Upstream != "" {
		return d.Upstream
	}
	return defaultUpstream
}

// Listen starts the UDP and TCP resolvers on the internal leg.
func (d *DNSRelay) Listen(ctx context.Context, ip string) error {
	pc, err := net.ListenPacket("udp", net.JoinHostPort(ip, "53"))
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(ip, "53"))
	if err != nil {
		pc.Close()
		return err
	}
	go func() { <-ctx.Done(); pc.Close(); ln.Close() }()
	go d.serveUDP(pc)
	go d.serveTCP(ln)
	d.Log.Info("dns relay listening", "address", ip, "max_in_flight", MaxDNSInFlight,
		"max_message_udp", MaxDNSMessage, "max_message_tcp", MaxDNSMessageTCP)
	return nil
}

func (d *DNSRelay) serveUDP(pc net.PacketConn) {
	sem := make(chan struct{}, MaxDNSInFlight)
	// One byte over the bound, so a datagram that does not fit is seen
	// not to fit. Reading into exactly MaxDNSMessage returns a full
	// buffer and no error while the kernel discards the rest, which is
	// indistinguishable from a message that happened to be that long.
	buf := make([]byte, MaxDNSMessage+1)
	for {
		n, client, err := pc.ReadFrom(buf)
		if err != nil {
			return // closed on shutdown
		}
		if n > MaxDNSMessage || !plausibleQuery(buf[:n]) {
			continue
		}
		query := make([]byte, n)
		copy(query, buf[:n])
		select {
		case sem <- struct{}{}:
		default:
			// At the in-flight bound: drop rather than queue. DNS is
			// already a lossy transport with client retries, so a drop
			// is the response a resolver under load is expected to
			// give, and queueing would only convert load into memory.
			continue
		}
		go func(query []byte, client net.Addr) {
			defer func() { <-sem }()
			answer, err := exchangeUDP(d.resolver(), query)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo(answer, client)
		}(query, client)
	}
}

func exchangeUDP(resolver string, query []byte) ([]byte, error) {
	up, err := net.Dial("udp", resolver)
	if err != nil {
		return nil, err
	}
	defer up.Close()
	_ = up.SetDeadline(time.Now().Add(DNSTimeout))
	if _, err := up.Write(query); err != nil {
		return nil, err
	}
	answer := make([]byte, MaxDNSMessage+1)
	n, err := up.Read(answer)
	if err != nil {
		return nil, err
	}
	if n > MaxDNSMessage {
		// Cut by this buffer, not by the resolver: the header still
		// claims the records that were lost and TC is not set, so
		// forwarding it hands the client a message that ends mid-record
		// and says it is whole. Dropping it leaves the client to retry
		// over TCP, which is what a truncated answer would have told it
		// to do anyway.
		return nil, errors.New("upstream answer does not fit a datagram")
	}
	if !plausibleQuery(answer[:n]) {
		return nil, errors.New("upstream answer is not a DNS message")
	}
	// The answer must belong to the query. The socket is connected, so
	// this is defence in depth rather than the primary check, but an
	// answer with a different ID is never correct to forward.
	if binary.BigEndian.Uint16(answer[:2]) != binary.BigEndian.Uint16(query[:2]) {
		return nil, errors.New("upstream answer has a different transaction id")
	}
	return answer[:n], nil
}

func (d *DNSRelay) serveTCP(ln net.Listener) {
	sem := make(chan struct{}, MaxDNSConns)
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		select {
		case sem <- struct{}{}:
		default:
			conn.Close()
			continue
		}
		go func(conn net.Conn) {
			defer func() { <-sem }()
			defer conn.Close()
			d.serveTCPConn(conn)
		}(conn)
	}
}

// serveTCPConn carries length-prefixed DNS messages. The prefix is
// honoured rather than trusted: a length above the bound is refused
// before any allocation, which is what keeps a two-byte header from
// asking for 64 KiB per connection.
func (d *DNSRelay) serveTCPConn(conn net.Conn) {
	up, err := net.DialTimeout("tcp", d.resolver(), DNSTimeout)
	if err != nil {
		return
	}
	defer up.Close()
	for {
		_ = conn.SetDeadline(time.Now().Add(DNSTimeout))
		query, err := readDNSMessage(conn)
		if err != nil {
			return
		}
		_ = up.SetDeadline(time.Now().Add(DNSTimeout))
		if err := writeDNSMessage(up, query); err != nil {
			return
		}
		answer, err := readDNSMessage(up)
		if err != nil {
			return
		}
		if binary.BigEndian.Uint16(answer[:2]) != binary.BigEndian.Uint16(query[:2]) {
			return
		}
		if err := writeDNSMessage(conn, answer); err != nil {
			return
		}
	}
}

func readDNSMessage(r io.Reader) ([]byte, error) {
	var prefix [2]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return nil, err
	}
	length := int(binary.BigEndian.Uint16(prefix[:]))
	if length < MinDNSMessage || length > MaxDNSMessageTCP {
		return nil, errors.New("dns message length out of bounds")
	}
	msg := make([]byte, length)
	if _, err := io.ReadFull(r, msg); err != nil {
		return nil, err
	}
	if !plausibleQuery(msg) {
		return nil, errors.New("not a dns message")
	}
	return msg, nil
}

func writeDNSMessage(w io.Writer, msg []byte) error {
	var prefix [2]byte
	binary.BigEndian.PutUint16(prefix[:], uint16(len(msg)))
	if _, err := w.Write(prefix[:]); err != nil {
		return err
	}
	_, err := w.Write(msg)
	return err
}

// plausibleQuery checks the shape a DNS message must have before the
// relay spends anything on it: a full header, and at least one question
// or answer to talk about. It is not a parser and does not pretend to
// validate the message body.
//
// The upper bound is the caller's, because it is a property of the
// transport rather than of the message: a datagram carries what the
// resolver agreed to, a TCP stream carries what its length prefix can
// frame, and the two differ by sixteen times.
func plausibleQuery(msg []byte) bool {
	if len(msg) < MinDNSMessage {
		return false
	}
	questions := binary.BigEndian.Uint16(msg[4:6])
	answers := binary.BigEndian.Uint16(msg[6:8])
	return questions > 0 || answers > 0
}
