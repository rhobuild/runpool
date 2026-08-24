package gateway

import (
	"bytes"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"
)

// header builds a minimal DNS message: transaction id, flags, and the
// four section counts.
func header(id uint16, questions, answers uint16, payload int) []byte {
	msg := make([]byte, MinDNSMessage+payload)
	binary.BigEndian.PutUint16(msg[0:2], id)
	binary.BigEndian.PutUint16(msg[4:6], questions)
	binary.BigEndian.PutUint16(msg[6:8], answers)
	return msg
}

// TestPlausibleQuery is the shape gate every message passes before the
// relay spends a socket or a goroutine on it. It is not a parser; it
// rejects what cannot be a DNS message at all.
func TestPlausibleQuery(t *testing.T) {
	if !plausibleQuery(header(1, 1, 0, 20)) {
		t.Error("a normal query was rejected")
	}
	if !plausibleQuery(header(1, 0, 1, 20)) {
		t.Error("an answer was rejected")
	}
	if plausibleQuery(header(1, 0, 0, 20)) {
		t.Error("a message with no questions and no answers was accepted")
	}
	if plausibleQuery(make([]byte, MinDNSMessage-1)) {
		t.Error("a truncated header was accepted")
	}
	if plausibleQuery(nil) {
		t.Error("an empty message was accepted")
	}
	// The upper bound belongs to the transport, not to the shape: a
	// datagram and a TCP stream carry different maxima, and this gate
	// runs on buffers the caller has already bounded.
	if !plausibleQuery(header(1, 1, 0, MaxDNSMessageTCP)) {
		t.Error("a message a TCP stream can legitimately frame was rejected on shape")
	}
}

// TestReadDNSMessageBounds: the TCP length prefix is honoured, never
// trusted. A prefix above the bound must be refused before anything is
// allocated — otherwise two bytes from a workload buy 64 KiB.
func TestReadDNSMessageBounds(t *testing.T) {
	frame := func(length int, body []byte) []byte {
		var buf bytes.Buffer
		var prefix [2]byte
		binary.BigEndian.PutUint16(prefix[:], uint16(length))
		buf.Write(prefix[:])
		buf.Write(body)
		return buf.Bytes()
	}

	good := header(7, 1, 0, 10)
	msg, err := readDNSMessage(bytes.NewReader(frame(len(good), good)))
	if err != nil || len(msg) != len(good) {
		t.Fatalf("a valid framed message failed: %v", err)
	}

	if _, err := readDNSMessage(bytes.NewReader(frame(3, []byte{1, 2, 3}))); err == nil {
		t.Error("an undersized length prefix was accepted")
	}
	// A prefix that promises more than the body delivers must fail
	// rather than return a short message. There is no case above the
	// bound to test: the prefix is two bytes, so MaxDNSMessageTCP is
	// every value it can hold, and the refusal that used to matter here
	// is now the datagram bound in exchangeUDP.
	if _, err := readDNSMessage(bytes.NewReader(frame(100, good))); err == nil {
		t.Error("a truncated body was accepted")
	}

	// The whole point of raising the TCP bound: an answer larger than a
	// datagram can carry is exactly what this transport is for.
	big := header(9, 1, 1, 20000)
	msg, err = readDNSMessage(bytes.NewReader(frame(len(big), big)))
	if err != nil {
		t.Fatalf("a %d byte answer over TCP failed: %v", len(big), err)
	}
	if len(msg) != len(big) {
		t.Errorf("read %d bytes of a %d byte answer", len(msg), len(big))
	}
}

// FuzzReadDNSMessage: whatever it accepts is within bounds and is a
// plausible message. It must never panic on hostile framing.
func FuzzReadDNSMessage(f *testing.F) {
	good := header(1, 1, 0, 10)
	var seed bytes.Buffer
	var prefix [2]byte
	binary.BigEndian.PutUint16(prefix[:], uint16(len(good)))
	seed.Write(prefix[:])
	seed.Write(good)
	f.Add(seed.Bytes())
	f.Add([]byte{0xff, 0xff})
	f.Add([]byte{0x00, 0x00})
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, in []byte) {
		msg, err := readDNSMessage(bytes.NewReader(in))
		if err != nil {
			return
		}
		if len(msg) < MinDNSMessage || len(msg) > MaxDNSMessageTCP {
			t.Fatalf("accepted a message of length %d", len(msg))
		}
		if !plausibleQuery(msg) {
			t.Fatalf("accepted an implausible message")
		}
	})
}

// TestWriteDNSMessageRoundTrip keeps the framing symmetric: what the
// relay writes, it can read back.
func TestWriteDNSMessageRoundTrip(t *testing.T) {
	msg := header(42, 1, 0, 30)
	var buf bytes.Buffer
	if err := writeDNSMessage(&buf, msg); err != nil {
		t.Fatal(err)
	}
	got, err := readDNSMessage(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, msg) {
		t.Error("the framed message did not round-trip")
	}
}

// answerWith starts a UDP resolver that echoes the query's transaction
// id and pads the answer to total bytes, so a test can ask for the
// answers a real resolver cannot be told to give.
func answerWith(t *testing.T, total int) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, MaxDNSMessage+1)
		for {
			n, client, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if n < MinDNSMessage {
				continue
			}
			answer := header(binary.BigEndian.Uint16(buf[:2]), 1, 1, total-MinDNSMessage)
			_, _ = pc.WriteTo(answer, client)
		}
	}()
	return pc.LocalAddr().String()
}

// TestUDPAnswerTooLargeForADatagramIsDropped. Reading into a buffer of
// exactly the bound returns a full buffer and no error while the kernel
// throws the tail away — so the relay would forward a message that ends
// mid-record with its header still claiming every record it had, and TC
// unset. The client would parse a lie. Dropping it leaves the retry to
// TCP, which is where an answer this size belongs.
func TestUDPAnswerTooLargeForADatagramIsDropped(t *testing.T) {
	query := header(0x1234, 1, 0, 30)

	fits := answerWith(t, MaxDNSMessage)
	answer, err := exchangeUDP(fits, query)
	if err != nil {
		t.Fatalf("an answer of exactly the bound was rejected: %v", err)
	}
	if len(answer) != MaxDNSMessage {
		t.Errorf("answer of %d bytes; want the full %d", len(answer), MaxDNSMessage)
	}

	oversized := answerWith(t, MaxDNSMessage+400)
	if answer, err := exchangeUDP(oversized, query); err == nil {
		t.Errorf("forwarded %d bytes of an answer that did not fit; it is cut mid-record "+
			"and its header still claims what was cut", len(answer))
	}
}

// TestTCPCarriesWhatADatagramCannot drives one whole relayed exchange
// over TCP with an answer four times the UDP bound. Capping TCP at the
// datagram size left large but ordinary answers unreachable by any
// transport: UDP truncates them and the fallback that exists to carry
// them refused them too, closing the connection with no reason given.
func TestTCPCarriesWhatADatagramCannot(t *testing.T) {
	const answerSize = 4 * MaxDNSMessage

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		query, err := readDNSMessage(conn)
		if err != nil {
			return
		}
		_ = writeDNSMessage(conn, header(binary.BigEndian.Uint16(query[:2]), 1, 1, answerSize-MinDNSMessage))
	}()

	d := &DNSRelay{Log: discardLogger(), Upstream: ln.Addr().String()}
	client, relay := net.Pipe()
	defer client.Close()
	go func() { defer relay.Close(); d.serveTCPConn(relay) }()

	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	if err := writeDNSMessage(client, header(0x4321, 1, 0, 40)); err != nil {
		t.Fatal(err)
	}
	answer, err := readDNSMessage(client)
	if err != nil {
		t.Fatalf("a %d byte answer did not survive the relay: %v", answerSize, err)
	}
	if len(answer) != answerSize {
		t.Errorf("relayed %d bytes of a %d byte answer", len(answer), answerSize)
	}
	if got := binary.BigEndian.Uint16(answer[:2]); got != 0x4321 {
		t.Errorf("answer transaction id = %#x; want the query's", got)
	}
}

// answerWithWrongID answers every query under a transaction id that is
// not the query's. Which is what the fixtures above cannot express: they
// copy the query's id into the answer, so the check that an answer
// belongs to its query was compared against a value derived from the
// thing under test — an assertion that cannot fail, over both checks
// that exist, on the one transport where the id is all that keeps
// pipelined answers matched to their queries.
func answerWithWrongID(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, MaxDNSMessage+1)
		for {
			n, client, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if n < MinDNSMessage {
				continue
			}
			answer := header(binary.BigEndian.Uint16(buf[:2])^0xffff, 1, 1, 0)
			_, _ = pc.WriteTo(answer, client)
		}
	}()
	return pc.LocalAddr().String()
}

// TestAnAnswerWithAnotherTransactionIDIsNeverForwarded, on both
// transports. UDP's connected socket makes this defence in depth; TCP's
// pipelining makes it the only thing keeping answers matched to queries.
func TestAnAnswerWithAnotherTransactionIDIsNeverForwarded(t *testing.T) {
	t.Run("udp", func(t *testing.T) {
		if _, err := exchangeUDP(answerWithWrongID(t), header(0x1234, 1, 0, 30)); err == nil {
			t.Fatal("an answer under another transaction id was forwarded")
		}
	})
	t.Run("tcp", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
		go func() {
			up, err := ln.Accept()
			if err != nil {
				return
			}
			defer up.Close()
			query, err := readDNSMessage(up)
			if err != nil {
				return
			}
			answer := header(binary.BigEndian.Uint16(query[:2])^0xffff, 1, 1, 0)
			_ = writeDNSMessage(up, answer)
		}()

		relay := &DNSRelay{Upstream: ln.Addr().String(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
		client, server := net.Pipe()
		t.Cleanup(func() { client.Close(); server.Close() })
		go relay.serveTCPConn(server)

		if err := writeDNSMessage(client, header(0x5678, 1, 0, 30)); err != nil {
			t.Fatal(err)
		}
		// The relay drops the connection rather than forwarding the
		// mismatched answer: the read ends without a message.
		client.SetReadDeadline(time.Now().Add(time.Second))
		if answer, err := readDNSMessage(client); err == nil {
			t.Fatalf("a mismatched answer was forwarded: id %#x for query 0x5678",
				binary.BigEndian.Uint16(answer[:2]))
		}
	})
}
