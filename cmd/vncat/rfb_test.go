package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"image"
	"image/color"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

type memoryConn struct {
	reader   *bytes.Reader
	mutex    sync.Mutex
	output   bytes.Buffer
	fragment int
	failure  error
}

func newMemoryConn(data []byte) *memoryConn      { return &memoryConn{reader: bytes.NewReader(data)} }
func (c *memoryConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
func (c *memoryConn) Write(p []byte) (int, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.failure != nil {
		return 0, c.failure
	}
	if c.fragment > 0 && len(p) > c.fragment {
		p = p[:c.fragment]
	}
	return c.output.Write(p)
}
func (c *memoryConn) bytes() []byte {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return bytes.Clone(c.output.Bytes())
}
func (c *memoryConn) Close() error                     { return nil }
func (c *memoryConn) LocalAddr() net.Addr              { return nil }
func (c *memoryConn) RemoteAddr() net.Addr             { return nil }
func (c *memoryConn) SetDeadline(time.Time) error      { return nil }
func (c *memoryConn) SetReadDeadline(time.Time) error  { return nil }
func (c *memoryConn) SetWriteDeadline(time.Time) error { return nil }

func serverInit(width, height uint16) []byte {
	data := make([]byte, 24)
	wireOrder.PutUint16(data, width)
	wireOrder.PutUint16(data[2:], height)
	copy(data[4:], []byte{32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 16, 8, 0, 0, 0, 0})
	return data
}

func handshakeData(minor int, security byte) []byte {
	version := map[int]string{3: "RFB 003.003\n", 7: "RFB 003.007\n", 8: "RFB 003.008\n"}[minor]
	data := []byte(version)
	if minor == 3 {
		data = append(data, 0, 0, 0, security)
	} else {
		data = append(data, 1, security)
	}
	if security == 2 {
		data = append(data, bytes.Repeat([]byte{42}, 16)...)
	}
	if minor == 8 || security == 2 {
		data = append(data, 0, 0, 0, 0)
	}
	return append(data, serverInit(4, 3)...)
}

func TestHandshakeVersionsAndAuthentication(t *testing.T) {
	for _, minor := range []int{3, 7, 8} {
		for _, security := range []byte{1, 2} {
			conn := newMemoryConn(handshakeData(minor, security))
			client := rfbClient{conn: conn}
			calls := 0
			secret := []byte("password")
			err := client.handshake(context.Background(), func() ([]byte, error) { calls++; return secret, nil })
			if err != nil {
				t.Fatalf("version %d security %d: %v", minor, security, err)
			}
			wantCalls := 0
			if security == 2 {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Fatalf("password calls %d", calls)
			}
			if security == 2 && !bytes.Equal(secret, make([]byte, 8)) {
				t.Fatal("password not cleared")
			}
			if client.frame.Rect != image.Rect(0, 0, 4, 3) {
				t.Fatal(client.frame.Rect)
			}
			output := conn.bytes()
			if string(output[:12]) != string(handshakeData(minor, security)[:12]) {
				t.Fatal("version negotiation")
			}
			offset := 12
			if minor != 3 {
				if output[offset] != security {
					t.Fatal("security selection")
				}
				offset++
			}
			if security == 2 {
				challenge := [16]byte{}
				copy(challenge[:], bytes.Repeat([]byte{42}, 16))
				want := challengeResponse([]byte("password"), challenge)
				if !bytes.Equal(output[offset:offset+16], want[:]) {
					t.Fatal("challenge response")
				}
				offset += 16
			}
			if output[offset] != 1 {
				t.Fatal("not shared")
			}
			offset++
			if !bytes.Equal(output[offset:offset+20], []byte{0, 0, 0, 0, 32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 16, 8, 0, 0, 0, 0}) {
				t.Fatal("pixel format")
			}
			offset += 20
			if output[offset] != 2 || wireOrder.Uint16(output[offset+2:]) != 5 {
				t.Fatal("encodings")
			}
		}
	}
}

func TestChallengeKnownVector(t *testing.T) {
	challengeBytes, _ := hex.DecodeString("a4513fa51f870610a45faebf4dac1222")
	var challenge [16]byte
	copy(challenge[:], challengeBytes)
	got := challengeResponse([]byte("Ch_#!T@8ignored"), challenge)
	if hex.EncodeToString(got[:]) != "71e4413043654e39da6d499343f65e29" {
		t.Fatalf("response %x", got)
	}
}

func TestHandshakeFailures(t *testing.T) {
	rejected := append([]byte("RFB 003.008\n\x01\x02"), make([]byte, 16)...)
	rejected = append(rejected, 0, 0, 0, 1, 0, 0, 0, 6)
	rejected = append(rejected, []byte("secret")...)
	oversized := append([]byte("RFB 003.003\n\x00\x00\x00\x00"), 0xff, 0xff, 0xff, 0xff)
	largeName := handshakeData(8, 1)
	copy(largeName[len(largeName)-4:], []byte{255, 255, 255, 255})
	largeFrame := handshakeData(8, 1)
	copy(largeFrame[18:22], []byte{255, 255, 255, 255})
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"version", []byte("RFB 004.000\n"), "unsupported protocol"},
		{"security33", []byte("RFB 003.003\n\x00\x00\x00\x13"), "unsupported security"},
		{"security38", []byte("RFB 003.008\n\x02\x13\x10"), "unsupported security"},
		{"authentication", rejected, "authentication failed"},
		{"oversized reason", oversized, "exceeds limit"},
		{"oversized name", largeName, "exceeds limit"},
		{"oversized framebuffer", largeFrame, "dimensions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := rfbClient{conn: newMemoryConn(tc.data)}
			err := c.handshake(context.Background(), func() ([]byte, error) { return []byte("secret"), nil })
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("error %v", err)
			}
		})
	}
	data := handshakeData(8, 2)
	for size := 0; size < len(data); size++ {
		c := rfbClient{conn: newMemoryConn(data[:size])}
		if err := c.handshake(context.Background(), func() ([]byte, error) { return []byte("pw"), nil }); err == nil {
			t.Fatalf("accepted truncated handshake %d", size)
		}
	}
	c := rfbClient{conn: newMemoryConn(data)}
	if err := c.handshake(context.Background(), func() ([]byte, error) { return nil, errors.New("secret") }); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal(err)
	}
}

func TestHandshakeCancellation(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		c := rfbClient{conn: client}
		done <- c.handshake(ctx, func() ([]byte, error) { return nil, nil })
	}()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("missing cancellation error")
		}
	case <-time.After(time.Second):
		t.Fatal("handshake leaked")
	}
}

func rectangle(x, y, width, height uint16, encoding int32, payload []byte) []byte {
	data := make([]byte, 12)
	wireOrder.PutUint16(data, x)
	wireOrder.PutUint16(data[2:], y)
	wireOrder.PutUint16(data[4:], width)
	wireOrder.PutUint16(data[6:], height)
	wireOrder.PutUint32(data[8:], uint32(encoding))
	return append(data, payload...)
}

func updateData(rects ...[]byte) []byte {
	data := []byte{0, byte(len(rects) >> 8), byte(len(rects))}
	for _, rect := range rects {
		data = append(data, rect...)
	}
	return data
}

func TestFrameEncodingsAndResize(t *testing.T) {
	data := updateData(
		rectangle(0, 0, 3, 1, 0, []byte{0, 0, 255, 0, 0, 255, 0, 0, 255, 0, 0, 0}),
		rectangle(1, 0, 3, 1, 1, []byte{0, 0, 0, 0}),
	)
	c := rfbClient{conn: newMemoryConn(data)}
	if err := c.resize(4, 2); err != nil {
		t.Fatal(err)
	}
	resized, err := c.update()
	if err != nil || resized {
		t.Fatal(resized, err)
	}
	want := []color.RGBA{{255, 0, 0, 255}, {255, 0, 0, 255}, {0, 255, 0, 255}, {0, 0, 255, 255}}
	for x, v := range want {
		if c.frame.RGBAAt(x, 0) != v {
			t.Fatalf("pixel %d: %v", x, c.frame.RGBAAt(x, 0))
		}
	}
	c.conn = newMemoryConn(updateData(rectangle(17, 19, 8, 5, -223, nil), rectangle(0, 0, 0, 0, -224, nil)))
	resized, err = c.update()
	if err != nil || !resized || c.frame.Rect != image.Rect(0, 0, 8, 5) {
		t.Fatal(resized, err, c.frame.Rect)
	}
	if c.frame.RGBAAt(0, 0) != want[0] {
		t.Fatal("desktop resize lost top-left pixels")
	}
}

func TestHextile(t *testing.T) {
	payload := []byte{2 | 4 | 8, 0, 0, 255, 0, 0, 255, 0, 0, 1, 0x11, 0x22, 0}
	c := rfbClient{conn: newMemoryConn(updateData(rectangle(0, 0, 20, 16, 5, payload)))}
	if err := c.resize(20, 16); err != nil {
		t.Fatal(err)
	}
	if _, err := c.update(); err != nil {
		t.Fatal(err)
	}
	if c.frame.RGBAAt(2, 2) != (color.RGBA{0, 255, 0, 255}) || c.frame.RGBAAt(19, 15) != (color.RGBA{255, 0, 0, 255}) {
		t.Fatal("hextile fill or carry")
	}
	c.conn = newMemoryConn(updateData(rectangle(0, 0, 2, 2, 5, []byte{2 | 8 | 16, 0, 0, 0, 0, 1, 255, 0, 0, 0, 0, 0x11})))
	if _, err := c.update(); err != nil {
		t.Fatal(err)
	}
	if c.frame.RGBAAt(1, 1) != (color.RGBA{0, 0, 255, 255}) {
		t.Fatal("colored subrect")
	}
}

func TestMalformedRectangles(t *testing.T) {
	cases := [][]byte{
		rectangle(3, 3, 2, 2, 0, nil), rectangle(0, 0, 0, 1, 0, nil),
		rectangle(0, 0, 1, 1, 100, nil), rectangle(0, 0, 1, 1, 0, []byte{1, 2, 3}),
		rectangle(0, 0, 2, 2, 1, []byte{0, 3, 0, 3}),
		rectangle(0, 0, 2, 2, 5, []byte{0}),
		rectangle(0, 0, 2, 2, 5, []byte{32}),
		rectangle(0, 0, 2, 2, 5, []byte{2 | 4 | 8, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0xff, 0xff}),
		rectangle(0, 0, 0, 2, -223, nil), rectangle(1, 0, 2, 0, -223, nil),
	}
	for i, data := range cases {
		c := rfbClient{conn: newMemoryConn(updateData(data))}
		if err := c.resize(4, 4); err != nil {
			t.Fatal(err)
		}
		if _, err := c.update(); err == nil {
			t.Fatalf("accepted malformed rectangle %d", i)
		}
	}
}

func TestStreamRequestsAndServerMessages(t *testing.T) {
	stream := []byte{2, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 3, 0, 0, 0, 0, 0, 0, 3, 'a', 'b', 'c', 0}
	stream = append(stream, updateData(rectangle(0, 0, 1, 1, 0, []byte{0, 0, 255, 0}))...)
	stream = append(stream, 0)
	stream = append(stream, updateData(rectangle(0, 0, 3, 2, -223, nil))...)
	conn := newMemoryConn(stream)
	c := rfbClient{conn: conn}
	if err := c.resize(2, 2); err != nil {
		t.Fatal(err)
	}
	frames := make(chan *image.RGBA, 2)
	if err := c.stream(context.Background(), frames); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	first, second := <-frames, <-frames
	if first.Rect.Dx() != 2 || first.RGBAAt(0, 0).R != 255 || second.Rect.Dx() != 3 {
		t.Fatal("bad snapshots")
	}
	output := conn.bytes()
	if len(output) != 30 || output[1] != 0 || output[11] != 1 || output[21] != 0 || wireOrder.Uint16(output[26:]) != 3 {
		t.Fatalf("requests %x", output)
	}
	for _, data := range [][]byte{{99}, {3, 0, 0, 0, 255, 255, 255, 255}, {1, 0, 255, 255, 0, 2}} {
		c.conn = newMemoryConn(data)
		if err := c.stream(context.Background(), frames); err == nil {
			t.Fatal("accepted invalid server message")
		}
	}
}

func TestSerializedShortWrites(t *testing.T) {
	conn := newMemoryConn(nil)
	conn.fragment = 1
	c := rfbClient{conn: conn}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := c.key(uint32(i), true); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	data := conn.bytes()
	if len(data) != 800 {
		t.Fatal(len(data))
	}
	seen := make(map[uint32]bool)
	for i := 0; i < len(data); i += 8 {
		if !bytes.Equal(data[i:i+4], []byte{4, 1, 0, 0}) {
			t.Fatalf("interleaved write %x", data[i:i+8])
		}
		seen[wireOrder.Uint32(data[i+4:])] = true
	}
	if len(seen) != 100 {
		t.Fatal("lost messages")
	}
	conn.failure = io.ErrClosedPipe
	if err := c.key(1, true); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
}
