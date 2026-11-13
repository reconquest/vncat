package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"io"
	"math"
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

func newMemoryConn(
	data []byte,
) *memoryConn {
	return &memoryConn{reader: bytes.NewReader(data)}
}

func (client *memoryConn) Read(
	p []byte,
) (int, error) {
	count, err := client.reader.Read(p)
	if err != nil {
		return count, fmt.Errorf("read memory connection: %w", err)
	}

	return count, nil
}

func (client *memoryConn) Write(data []byte) (int, error) {
	client.mutex.Lock()
	defer client.mutex.Unlock()

	if client.failure != nil {
		return 0, client.failure
	}

	if client.fragment > 0 && len(data) > client.fragment {
		data = data[:client.fragment]
	}

	count, err := client.output.Write(data)
	if err != nil {
		return count, fmt.Errorf("write memory connection: %w", err)
	}

	return count, nil
}

func (client *memoryConn) bytes() []byte {
	client.mutex.Lock()
	defer client.mutex.Unlock()

	return bytes.Clone(client.output.Bytes())
}
func (*memoryConn) Close() error                     { return nil }
func (*memoryConn) LocalAddr() net.Addr              { return nil }
func (*memoryConn) RemoteAddr() net.Addr             { return nil }
func (*memoryConn) SetDeadline(time.Time) error      { return nil }
func (*memoryConn) SetReadDeadline(time.Time) error  { return nil }
func (*memoryConn) SetWriteDeadline(time.Time) error { return nil }

func serverInit(width, height uint16) []byte {
	var header [24]byte

	data := header[:]
	binary.BigEndian.PutUint16(data, width)
	binary.BigEndian.PutUint16(data[2:], height)
	copy(
		data[4:],
		[]byte{32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 16, 8, 0, 0, 0, 0},
	)

	return data
}

func handshakeData(minor int, security byte) []byte {
	version := map[int]string{
		3: "RFB 003.003\n",
		7: "RFB 003.007\n",
		8: "RFB 003.008\n",
	}[minor]
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

			err := client.handshake(t.Context(), func() ([]byte, error) {
				calls++

				return secret, nil
			})
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
			if !bytes.Equal(output[:12], handshakeData(minor, security)[:12]) {
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
			if !bytes.Equal(
				output[offset:offset+20],
				[]byte{
					0,
					0,
					0,
					0,
					32,
					24,
					0,
					1,
					0,
					255,
					0,
					255,
					0,
					255,
					16,
					8,
					0,
					0,
					0,
					0,
				},
			) {
				t.Fatal("pixel format")
			}

			offset += 20
			if output[offset] != 2 ||
				binary.BigEndian.Uint16(output[offset+2:]) != 5 {
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

	oversized := append(
		[]byte("RFB 003.003\n\x00\x00\x00\x00"),
		0xff,
		0xff,
		0xff,
		0xff,
	)
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
		{
			"security33",
			[]byte("RFB 003.003\n\x00\x00\x00\x13"),
			"unsupported security",
		},
		{
			"security38",
			[]byte("RFB 003.008\n\x02\x13\x10"),
			"unsupported security",
		},
		{"authentication", rejected, "authentication failed"},
		{"oversized reason", oversized, "exceeds limit"},
		{"oversized name", largeName, "exceeds limit"},
		{"oversized framebuffer", largeFrame, "dimensions"},
	}
	for _, testcase := range cases {
		t.Run(testcase.name, func(t *testing.T) {
			client := rfbClient{conn: newMemoryConn(testcase.data)}
			err := client.handshake(
				t.Context(),
				func() ([]byte, error) { return []byte("secret"), nil },
			)

			if err == nil || !strings.Contains(err.Error(), testcase.want) ||
				strings.Contains(err.Error(), "secret") {
				t.Fatalf("error %v", err)
			}
		})
	}

	data := handshakeData(8, 2)
	for size := range data {
		client := rfbClient{conn: newMemoryConn(data[:size])}

		err := client.handshake(
			t.Context(),
			func() ([]byte, error) { return []byte("pw"), nil },
		)
		if err == nil {
			t.Fatalf("accepted truncated handshake %d", size)
		}
	}

	client := rfbClient{conn: newMemoryConn(data)}
	err := client.handshake(
		t.Context(),
		func() ([]byte, error) { return nil, errors.New("secret") },
	)

	if err == nil ||
		strings.Contains(err.Error(), "secret") {
		t.Fatal(err)
	}
}

func TestHandshakeCancellation(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() {
		client := rfbClient{conn: client}
		done <- client.handshake(ctx, func() ([]byte, error) {
			return nil, nil
		})
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

func rectangle(
	column, row, width, height uint16,
	encoding int32,
	payload []byte,
) []byte {
	var header [12]byte

	data := header[:]
	binary.BigEndian.PutUint16(data, column)
	binary.BigEndian.PutUint16(data[2:], row)
	binary.BigEndian.PutUint16(data[4:], width)
	binary.BigEndian.PutUint16(data[6:], height)

	bits := uint32(encoding & math.MaxInt32)
	if encoding < 0 {
		bits |= 1 << 31
	}

	binary.BigEndian.PutUint32(data[8:], bits)

	return append(data, payload...)
}

func updateData(rects ...[]byte) []byte {
	data := []byte{
		0,
		byte((len(rects) >> 8) & math.MaxUint8),
		byte(len(rects) & math.MaxUint8),
	}
	for _, rect := range rects {
		data = append(data, rect...)
	}

	return data
}

func TestFrameEncodingsAndResize(t *testing.T) {
	data := updateData(
		rectangle(
			0,
			0,
			3,
			1,
			0,
			[]byte{0, 0, 255, 0, 0, 255, 0, 0, 255, 0, 0, 0},
		),
		rectangle(1, 0, 3, 1, 1, []byte{0, 0, 0, 0}),
	)
	client := rfbClient{conn: newMemoryConn(data)}

	resizeError := client.resize(4, 2)
	if resizeError != nil {
		t.Fatal(resizeError)
	}

	resized, err := client.update()
	if err != nil || resized {
		t.Fatal(resized, err)
	}

	want := []color.RGBA{
		{255, 0, 0, 255},
		{255, 0, 0, 255},
		{0, 255, 0, 255},
		{0, 0, 255, 255},
	}
	for x, v := range want {
		if client.frame.RGBAAt(x, 0) != v {
			t.Fatalf("pixel %d: %v", x, client.frame.RGBAAt(x, 0))
		}
	}

	client.conn = newMemoryConn(
		updateData(
			rectangle(17, 19, 8, 5, -223, nil),
			rectangle(0, 0, 0, 0, -224, nil),
		),
	)
	resized, err = client.update()

	if err != nil || !resized || client.frame.Rect != image.Rect(0, 0, 8, 5) {
		t.Fatal(resized, err, client.frame.Rect)
	}

	if client.frame.RGBAAt(0, 0) != want[0] {
		t.Fatal("desktop resize lost top-left pixels")
	}
}

func TestHextile(t *testing.T) {
	payload := []byte{2 | 4 | 8, 0, 0, 255, 0, 0, 255, 0, 0, 1, 0x11, 0x22, 0}
	client := rfbClient{
		conn: newMemoryConn(updateData(rectangle(0, 0, 20, 16, 5, payload))),
	}

	resizeError := client.resize(20, 16)
	if resizeError != nil {
		t.Fatal(resizeError)
	}

	_, updateError := client.update()
	if updateError != nil {
		t.Fatal(updateError)
	}

	if client.frame.RGBAAt(2, 2) != (color.RGBA{0, 255, 0, 255}) ||
		client.frame.RGBAAt(19, 15) != (color.RGBA{255, 0, 0, 255}) {
		t.Fatal("hextile fill or carry")
	}

	client.conn = newMemoryConn(
		updateData(
			rectangle(
				0,
				0,
				2,
				2,
				5,
				[]byte{2 | 8 | 16, 0, 0, 0, 0, 1, 255, 0, 0, 0, 0, 0x11},
			),
		),
	)

	_, updateError = client.update()
	if updateError != nil {
		t.Fatal(updateError)
	}

	if client.frame.RGBAAt(1, 1) != (color.RGBA{0, 0, 255, 255}) {
		t.Fatal("colored subrect")
	}
}

func TestMalformedRectangles(t *testing.T) {
	cases := [][]byte{
		rectangle(3, 3, 2, 2, 0, nil),
		rectangle(0, 0, 0, 1, 0, nil),
		rectangle(
			0,
			0,
			1,
			1,
			100,
			nil,
		),
		rectangle(0, 0, 1, 1, 0, []byte{1, 2, 3}),
		rectangle(0, 0, 2, 2, 1, []byte{0, 3, 0, 3}),
		rectangle(0, 0, 2, 2, 5, []byte{0}),
		rectangle(0, 0, 2, 2, 5, []byte{32}),
		rectangle(
			0,
			0,
			2,
			2,
			5,
			[]byte{2 | 4 | 8, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0xff, 0xff},
		),
		rectangle(0, 0, 0, 2, -223, nil),
		rectangle(1, 0, 2, 0, -223, nil),
	}
	for index, data := range cases {
		client := rfbClient{conn: newMemoryConn(updateData(data))}

		resizeError := client.resize(4, 4)
		if resizeError != nil {
			t.Fatal(resizeError)
		}

		_, updateError := client.update()
		if updateError == nil {
			t.Fatalf("accepted malformed rectangle %d", index)
		}
	}
}

func TestStreamRequestsAndServerMessages(t *testing.T) {
	stream := make([]byte, 0, 60)

	stream = append(stream,
		2,
		1,
		0,
		0,
		0,
		0,
		1,
		0,
		0,
		0,
		0,
		0,
		0,
		3,
		0,
		0,
		0,
		0,
		0,
		0,
		3,
		'a',
		'b',
		'c',
		0,
	)

	stream = append(
		stream,
		updateData(rectangle(0, 0, 1, 1, 0, []byte{0, 0, 255, 0}))...)
	stream = append(stream, 0)
	stream = append(stream, updateData(rectangle(0, 0, 3, 2, -223, nil))...)

	conn := newMemoryConn(stream)
	client := rfbClient{conn: conn}

	resizeError := client.resize(2, 2)
	if resizeError != nil {
		t.Fatal(resizeError)
	}

	frames := make(chan *image.RGBA, 2)

	resizeError = client.stream(t.Context(), frames)

	if !errors.Is(resizeError, io.EOF) {
		t.Fatal(resizeError)
	}

	first, second := <-frames, <-frames
	if first.Rect.Dx() != 2 || first.RGBAAt(0, 0).R != 255 ||
		second.Rect.Dx() != 3 {
		t.Fatal("bad snapshots")
	}

	output := conn.bytes()
	if len(output) != 30 || output[1] != 0 || output[11] != 1 ||
		output[21] != 0 ||
		binary.BigEndian.Uint16(output[26:]) != 3 {
		t.Fatalf("requests %x", output)
	}

	for _, data := range [][]byte{
		{99},
		{3, 0, 0, 0, 255, 255, 255, 255},
		{1, 0, 255, 255, 0, 2},
	} {
		client.conn = newMemoryConn(data)

		err := client.stream(t.Context(), frames)
		if err == nil {
			t.Fatal("accepted invalid server message")
		}
	}
}

func TestSerializedShortWrites(t *testing.T) {
	conn := newMemoryConn(nil)

	conn.fragment = 1

	client := rfbClient{conn: conn}

	var workers sync.WaitGroup

	for index := range uint32(100) {
		workers.Go(func() {
			err := client.key(index, true)
			if err != nil {
				t.Error(err)
			}
		})
	}

	workers.Wait()

	data := conn.bytes()
	if len(data) != 800 {
		t.Fatal(len(data))
	}

	seen := make(map[uint32]bool)

	for i := 0; i < len(data); i += 8 {
		if !bytes.Equal(data[i:i+4], []byte{4, 1, 0, 0}) {
			t.Fatalf("interleaved write %x", data[i:i+8])
		}

		seen[binary.BigEndian.Uint32(data[i+4:])] = true
	}

	if len(seen) != 100 {
		t.Fatal("lost messages")
	}

	conn.failure = io.ErrClosedPipe

	err := client.key(1, true)

	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
}

func TestPasswordErrorsPreserveSafeCancellation(t *testing.T) {
	for _, sentinel := range []error{
		context.Canceled,
		context.DeadlineExceeded,
		io.ErrClosedPipe,
	} {
		client := rfbClient{conn: newMemoryConn(handshakeData(8, 2))}
		secret := []byte("secret")
		err := client.handshake(t.Context(), func() ([]byte, error) {
			return secret, errors.Join(errors.New("secret"), sentinel)
		})

		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe password error: %v", err)
		}

		if !errors.Is(sentinel, io.ErrClosedPipe) && !errors.Is(err, sentinel) {
			t.Fatalf("lost cancellation: %v", err)
		}

		if !bytes.Equal(secret, make([]byte, len(secret))) {
			t.Fatal("failed password not cleared")
		}
	}
}

func TestUnknownVersionDowngrade(t *testing.T) {
	for _, version := range []string{
		"RFB 003.889\n", "RFB 003.000\n", "RFB 003.009\n",
	} {
		for _, security := range []byte{1, 2} {
			data := handshakeData(3, security)
			copy(data, version)

			conn := newMemoryConn(data)
			client := rfbClient{conn: conn}

			err := client.handshake(t.Context(), func() ([]byte, error) {
				return []byte("password"), nil
			})
			if err != nil {
				t.Fatal(version, err)
			}

			if string(conn.bytes()[:12]) != "RFB 003.003\n" ||
				client.frame.Rect != image.Rect(0, 0, 4, 3) {
				t.Fatal("failed legacy negotiation", version)
			}
		}
	}

	for _, version := range []string{
		"RFB 004.008\n", "RFB 003.a89\n", "RFB 003.889!",
		"rfb 003.889\n", "RFB 003. 89\n",
	} {
		conn := newMemoryConn([]byte(version))
		client := rfbClient{conn: conn}
		err := client.handshake(t.Context(), func() ([]byte, error) {
			t.Fatal("invalid banner requested password")

			return nil, nil
		})

		if err == nil || len(conn.bytes()) != 0 {
			t.Fatal("accepted invalid banner", version)
		}
	}
}
