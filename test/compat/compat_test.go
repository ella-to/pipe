// Package compat proves that a pipe connection is an ordinary net.Conn as far
// as the standard library is concerned. Every test here drives a real Pion
// PeerConnection over in-process signaling and host candidates, so it needs no
// network, Docker, or privileges.
//
// These are compatibility claims, not benchmarks: each test asserts exact bytes,
// checksums, or decoded values.
package compat

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/gob"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"ella.to/pipe"
	"ella.to/pipe/internal/testutil"
	"ella.to/pipe/signaling/memory"
)

const testTimeout = 30 * time.Second

// harness is a dialing endpoint and a listening endpoint on one in-process hub.
type harness struct {
	dialer *pipe.Endpoint
	server *pipe.Endpoint
	ln     net.Listener
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	hub := memory.New()
	h := &harness{
		dialer: newEndpoint(t, hub, "client"),
		server: newEndpoint(t, hub, "server"),
	}

	ln, err := h.server.Listen()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	h.ln = ln
	t.Cleanup(func() { _ = ln.Close() })
	return h
}

func newEndpoint(t *testing.T, hub *memory.Hub, id pipe.PeerID) *pipe.Endpoint {
	t.Helper()

	ep, err := pipe.New(context.Background(), pipe.Config{
		ID:          id,
		Signaler:    hub,
		DialTimeout: testTimeout,
	})
	if err != nil {
		t.Fatalf("new endpoint %s: %v", id, err)
	}
	t.Cleanup(func() { _ = ep.Close() })
	return ep
}

// dial returns both ends of one connection.
func (h *harness) dial(t *testing.T) (client, server net.Conn) {
	t.Helper()

	type accepted struct {
		conn net.Conn
		err  error
	}
	acceptCh := make(chan accepted, 1)
	go func() {
		c, err := h.ln.Accept()
		acceptCh <- accepted{c, err}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	c, err := h.dialer.Dial(ctx, "server")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	select {
	case a := <-acceptCh:
		if a.err != nil {
			t.Fatalf("accept: %v", a.err)
		}
		t.Cleanup(func() { _ = a.conn.Close() })
		return c, a.conn
	case <-time.After(testTimeout):
		t.Fatal("accept timed out")
		return nil, nil
	}
}

// TestIOCopy checks io.Copy in both directions across the payload sizes normal
// CI covers, verifying the length and the SHA-256 of what arrived.
func TestIOCopy(t *testing.T) {
	testutil.CheckLeaks(t)

	sizes := []int{1, 1 << 10, 1 << 20}
	if !testing.Short() {
		sizes = append(sizes, 8<<20)
	}

	h := newHarness(t)

	for _, size := range sizes {
		t.Run(fmt.Sprintf("%dB", size), func(t *testing.T) {
			client, server := h.dial(t)

			payload := make([]byte, size)
			if _, err := rand.Read(payload); err != nil {
				t.Fatal(err)
			}
			want := sha256.Sum256(payload)

			// The server echoes with io.Copy, which is the loop most callers
			// actually write.
			echoed := make(chan error, 1)
			go func() {
				_, err := io.Copy(server, server)
				echoed <- err
			}()

			written := make(chan error, 1)
			go func() {
				n, err := io.Copy(client, bytes.NewReader(payload))
				if err == nil && n != int64(size) {
					err = fmt.Errorf("wrote %d bytes, want %d", n, size)
				}
				// Closing the write side is not available, so the reader below
				// stops at the expected length instead.
				written <- err
			}()

			got := make([]byte, size)
			if err := client.SetReadDeadline(time.Now().Add(testTimeout)); err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadFull(client, got); err != nil {
				t.Fatalf("read back: %v", err)
			}
			if err := <-written; err != nil {
				t.Fatalf("write: %v", err)
			}
			if sum := sha256.Sum256(got); sum != want {
				t.Fatal("the echoed bytes do not match what was sent")
			}

			_ = server.Close()
			<-echoed
		})
	}
}

// TestBufio proves buffered line-oriented protocols work, including a
// bufio.Scanner on the far side of a frame boundary.
func TestBufio(t *testing.T) {
	testutil.CheckLeaks(t)

	h := newHarness(t)
	client, server := h.dial(t)

	const lines = 2000

	// The server upper-cases every line it reads.
	go func() {
		r := bufio.NewReader(server)
		w := bufio.NewWriter(server)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if _, err := w.WriteString(strings.ToUpper(line)); err != nil {
				return
			}
			if err := w.Flush(); err != nil {
				return
			}
		}
	}()

	if err := client.SetDeadline(time.Now().Add(testTimeout)); err != nil {
		t.Fatal(err)
	}

	w := bufio.NewWriter(client)
	scanner := bufio.NewScanner(client)

	for i := range lines {
		// A long line crosses the 16 KiB frame payload limit, which is where a
		// framing bug would show up.
		body := strings.Repeat("x", i%64)
		if i == lines/2 {
			body = strings.Repeat("y", 40<<10)
		}
		if _, err := fmt.Fprintf(w, "line-%d-%s\n", i, body); err != nil {
			t.Fatalf("write line %d: %v", i, err)
		}
		if err := w.Flush(); err != nil {
			t.Fatalf("flush line %d: %v", i, err)
		}

		if !scanner.Scan() {
			t.Fatalf("read line %d: %v", i, scanner.Err())
		}
		want := strings.ToUpper(fmt.Sprintf("line-%d-%s", i, body))
		if got := scanner.Text(); got != want {
			t.Fatalf("line %d = %.32q..., want %.32q...", i, got, want)
		}
	}
}

// TestJSONStream proves a json.Decoder can read a stream of values without
// over-reading past a value boundary.
func TestJSONStream(t *testing.T) {
	testutil.CheckLeaks(t)

	type message struct {
		Seq  int    `json:"seq"`
		Text string `json:"text"`
		Blob []byte `json:"blob"`
	}

	h := newHarness(t)
	client, server := h.dial(t)

	const count = 500

	sendErr := make(chan error, 1)
	go func() {
		enc := json.NewEncoder(server)
		for i := range count {
			msg := message{
				Seq:  i,
				Text: strings.Repeat("t", i%128),
				Blob: bytes.Repeat([]byte{byte(i)}, i%256),
			}
			if err := enc.Encode(msg); err != nil {
				sendErr <- err
				return
			}
		}
		sendErr <- nil
	}()

	if err := client.SetReadDeadline(time.Now().Add(testTimeout)); err != nil {
		t.Fatal(err)
	}

	dec := json.NewDecoder(client)
	for i := range count {
		var got message
		if err := dec.Decode(&got); err != nil {
			t.Fatalf("decode %d: %v", i, err)
		}
		if got.Seq != i {
			t.Fatalf("seq = %d, want %d", got.Seq, i)
		}
		if len(got.Text) != i%128 || len(got.Blob) != i%256 {
			t.Fatalf("message %d arrived with the wrong lengths: %d/%d", i, len(got.Text), len(got.Blob))
		}
	}
	if err := <-sendErr; err != nil {
		t.Fatalf("encode: %v", err)
	}
}

// TestGob proves encoding/gob works, which matters because gob is
// stream-stateful: a single lost or duplicated byte breaks every later value.
func TestGob(t *testing.T) {
	testutil.CheckLeaks(t)

	type record struct {
		Name   string
		Values []float64
		Nested map[string]int
	}

	h := newHarness(t)
	client, server := h.dial(t)

	const count = 200

	go func() {
		enc := gob.NewEncoder(server)
		for i := range count {
			_ = enc.Encode(record{
				Name:   fmt.Sprintf("record-%d", i),
				Values: []float64{float64(i), float64(i) * 1.5},
				Nested: map[string]int{"i": i},
			})
		}
	}()

	if err := client.SetReadDeadline(time.Now().Add(testTimeout)); err != nil {
		t.Fatal(err)
	}

	dec := gob.NewDecoder(client)
	for i := range count {
		var got record
		if err := dec.Decode(&got); err != nil {
			t.Fatalf("decode %d: %v", i, err)
		}
		if want := fmt.Sprintf("record-%d", i); got.Name != want {
			t.Fatalf("name = %q, want %q", got.Name, want)
		}
		if got.Nested["i"] != i || len(got.Values) != 2 {
			t.Fatalf("record %d decoded as %+v", i, got)
		}
	}
}

// TestTLS runs a full TLS 1.3 handshake and transfer over a pipe connection.
// TLS is the strictest compatibility test in the suite: it needs exact byte
// ordering, and it fails loudly on any duplication or truncation.
func TestTLS(t *testing.T) {
	testutil.CheckLeaks(t)

	cert, pool := selfSigned(t, "pipe.test")

	h := newHarness(t)
	client, server := h.dial(t)

	serverTLS := tls.Server(server, &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	})
	clientTLS := tls.Client(client, &tls.Config{
		RootCAs:    pool,
		ServerName: "pipe.test",
		MinVersion: tls.VersionTLS13,
	})

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	var (
		wg               sync.WaitGroup
		clientErr, srvEr error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		srvEr = serverTLS.HandshakeContext(ctx)
	}()
	go func() {
		defer wg.Done()
		clientErr = clientTLS.HandshakeContext(ctx)
	}()
	wg.Wait()

	if srvEr != nil {
		t.Fatalf("server handshake: %v", srvEr)
	}
	if clientErr != nil {
		t.Fatalf("client handshake: %v", clientErr)
	}
	if state := clientTLS.ConnectionState(); state.Version != tls.VersionTLS13 {
		t.Errorf("negotiated version = %x, want TLS 1.3", state.Version)
	}

	// A payload larger than one TLS record and one pipe frame.
	payload := make([]byte, 300<<10)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(payload)

	go func() {
		defer serverTLS.Close()
		_, _ = io.Copy(serverTLS, serverTLS)
	}()

	if err := client.SetDeadline(time.Now().Add(testTimeout)); err != nil {
		t.Fatal(err)
	}

	written := make(chan error, 1)
	go func() {
		_, err := clientTLS.Write(payload)
		written <- err
	}()

	got := make([]byte, len(payload))
	if _, err := io.ReadFull(clientTLS, got); err != nil {
		t.Fatalf("read back over TLS: %v", err)
	}
	if err := <-written; err != nil {
		t.Fatalf("write over TLS: %v", err)
	}
	if sha256.Sum256(got) != want {
		t.Fatal("the TLS payload did not survive the round trip")
	}
	_ = clientTLS.Close()
}

// TestHTTP serves HTTP/1.1 over a pipe listener and drives it with a stock
// http.Client whose transport dials through the endpoint. Connection reuse
// across requests is part of what is being checked.
func TestHTTP(t *testing.T) {
	testutil.CheckLeaks(t)

	h := newHarness(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(body)
	})
	mux.HandleFunc("/large", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("z"), 512<<10))
	})

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: testTimeout,
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(h.ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		<-served
	})

	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			// The address is ignored: a peer ID is the destination.
			return h.dialer.Dial(ctx, "server")
		},
		MaxIdleConns:    2,
		IdleConnTimeout: 30 * time.Second,
	}
	t.Cleanup(transport.CloseIdleConnections)

	client := &http.Client{Transport: transport, Timeout: testTimeout}

	// Several requests in sequence exercise keep-alive over one pipe conn.
	for i := range 5 {
		body := strings.Repeat(fmt.Sprintf("request-%d;", i), 100)
		resp, err := client.Post("http://server/echo", "text/plain", strings.NewReader(body))
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		got, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("read body %d: %v", i, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d status = %d", i, resp.StatusCode)
		}
		if string(got) != body {
			t.Fatalf("request %d echoed %d bytes, want %d", i, len(got), len(body))
		}
	}

	resp, err := client.Get("http://server/large")
	if err != nil {
		t.Fatalf("large request: %v", err)
	}
	n, err := io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("large body: %v", err)
	}
	if n != 512<<10 {
		t.Fatalf("large body = %d bytes, want %d", n, 512<<10)
	}
}

// selfSigned returns a certificate for name and a pool that trusts it.
func selfSigned(t *testing.T, name string) (tls.Certificate, *x509.CertPool) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		DNSNames:              []string{name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatal("the generated certificate was not accepted into a pool")
	}
	return cert, pool
}
