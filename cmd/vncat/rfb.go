package main

import (
	"context"
	"crypto/des"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"io"
	"math"
	"math/bits"
	"net"
	"slices"
	"sync"
	"time"
)

const (
	maxPixels = 16 * 1024 * 1024
	maxText   = 1024 * 1024
)

type rfbClient struct {
	conn        net.Conn
	writes      sync.Mutex
	readonly    bool
	singleFrame bool
	hasPixels   bool
	frame       *image.RGBA
}

func (client *rfbClient) handshake(
	ctx context.Context,
	password func() ([]byte, error),
) error {
	err := client.conn.SetDeadline(
		time.Now().Add(handshakeSeconds * time.Second),
	)
	if err != nil {
		return fmt.Errorf("set handshake deadline: %w", err)
	}

	stopped := make(chan struct{})
	stop := context.AfterFunc(
		ctx,
		func() { _ = client.conn.SetDeadline(time.Now()); close(stopped) },
	)

	defer func() {
		if !stop() {
			<-stopped
		}

		_ = client.conn.SetDeadline(time.Time{})
	}()

	err = client.negotiate(password)
	if err != nil {
		return fmt.Errorf("RFB handshake: %w", err)
	}

	return nil
}

func (client *rfbClient) negotiate(password func() ([]byte, error)) error {
	var version [12]byte

	_, readFullError := io.ReadFull(client.conn, version[:])
	if readFullError != nil {
		return fmt.Errorf("read version: %w", readFullError)
	}

	var minor int

	switch string(version[:]) {
	case "RFB 003.003\n":
		minor = 3

	case "RFB 003.007\n":
		minor = 7

	case "RFB 003.008\n":
		minor = 8

	default:
		if !legacyVersion(version) {
			return fmt.Errorf("unsupported protocol version %q", version)
		}

		minor = 3

		copy(version[:], "RFB 003.003\n")
	}

	writeError := client.write(version[:])
	if writeError != nil {
		return writeError
	}

	return client.negotiateSecurity(minor, password)
}

func (client *rfbClient) negotiateSecurity(
	minor int,
	password func() ([]byte, error),
) error {
	security, err := client.security(minor)
	if err != nil {
		return err
	}

	err = client.completeAuthentication(security, minor, password)
	if err != nil {
		return err
	}

	return client.initialize()
}

func (client *rfbClient) completeAuthentication(
	security uint32,
	minor int,
	password func() ([]byte, error),
) error {
	if security == securityVNC {
		err := client.authenticate(password)
		if err != nil {
			return err
		}
	}

	if security == 2 || minor == 8 {
		err := client.authenticationResult(minor)
		if err != nil {
			return err
		}
	}

	return nil
}

func (client *rfbClient) initialize() error {
	writeError := client.write([]byte{1})
	if writeError != nil {
		return writeError
	}

	var init [24]byte

	_, readFullError := io.ReadFull(client.conn, init[:])
	if readFullError != nil {
		return fmt.Errorf("read server initialization: %w", readFullError)
	}

	width, height := int(
		binary.BigEndian.Uint16(init[:2]),
	), int(
		binary.BigEndian.Uint16(init[2:4]),
	)

	writeError = client.resize(width, height)
	if writeError != nil {
		return writeError
	}

	writeError = discardSized(client.conn, binary.BigEndian.Uint32(init[20:24]))
	if writeError != nil {
		return fmt.Errorf("desktop name: %w", writeError)
	}

	return client.configurePixels()
}

func (client *rfbClient) configurePixels() error {
	format := []byte{
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
	}

	writeError := client.write(format)
	if writeError != nil {
		return writeError
	}

	encodings := [...]uint32{
		encodingCopyRect,
		encodingHextile,
		encodingRaw,
		encodingDesktopSize,
		encodingLastRect,
	}
	msg := []byte{2, 0, 0, 5}

	for _, encoding := range encodings {
		msg = binary.BigEndian.AppendUint32(msg, encoding)
	}

	return client.write(msg)
}

func (client *rfbClient) security(minor int) (uint32, error) {
	if minor == protocolMinorLegacy {
		return client.legacySecurity()
	}

	return client.selectSecurity()
}

func (client *rfbClient) legacySecurity() (uint32, error) {
	var security uint32

	err := binary.Read(client.conn, binary.BigEndian, &security)
	if err != nil {
		return 0, fmt.Errorf("read security type: %w", err)
	}

	if security == 0 {
		return 0, client.rejected()
	}

	if security != 1 && security != 2 {
		return 0, fmt.Errorf(
			"unsupported security type %d (supported: None, VNC password)",
			security,
		)
	}

	return security, nil
}

func (client *rfbClient) selectSecurity() (uint32, error) {
	var count [1]byte

	_, readFullError := io.ReadFull(client.conn, count[:])
	if readFullError != nil {
		return 0, fmt.Errorf("read security types: %w", readFullError)
	}

	if count[0] == 0 {
		return 0, client.rejected()
	}

	var available [255]byte

	types := available[:int(count[0])]

	_, readFullError = io.ReadFull(client.conn, types)
	if readFullError != nil {
		return 0, fmt.Errorf("read security types: %w", readFullError)
	}

	for _, preferred := range []byte{2, 1} {
		if slices.Contains(types, preferred) {
			return uint32(preferred), client.write([]byte{preferred})
		}
	}

	return 0, fmt.Errorf(
		"unsupported security types %v (supported: None, VNC password)",
		types,
	)
}

func (client *rfbClient) rejected() error {
	err := discardText(client.conn)
	if err != nil {
		return fmt.Errorf("server rejected connection: %w", err)
	}

	return errors.New("server rejected connection by security policy")
}

func challengeResponse(password []byte, challenge [16]byte) [16]byte {
	var key [8]byte

	copy(key[:], password)

	for i := range key {
		key[i] = bits.Reverse8(key[i])
	}

	cipher, _ := des.NewCipher(key[:])
	clear(key[:])

	var response [16]byte

	cipher.Encrypt(response[:8], challenge[:8])
	cipher.Encrypt(response[8:], challenge[8:])

	return response
}

func discardText(reader io.Reader) error {
	var size uint32

	err := binary.Read(reader, binary.BigEndian, &size)
	if err != nil {
		return fmt.Errorf("read text length: %w", err)
	}

	return discardSized(reader, size)
}

func discardSized(reader io.Reader, size uint32) error {
	if size > maxText {
		return fmt.Errorf("text length %d exceeds limit", size)
	}

	_, err := io.CopyN(io.Discard, reader, int64(size))
	if err != nil {
		return fmt.Errorf("discard text: %w", err)
	}

	return nil
}

func (client *rfbClient) write(data []byte) error {
	client.writes.Lock()
	defer client.writes.Unlock()

	deadlineError := client.conn.SetWriteDeadline(
		time.Now().Add(writeSeconds * time.Second),
	)
	if deadlineError != nil {
		return fmt.Errorf("set write deadline: %w", deadlineError)
	}

	for len(data) > 0 {
		written, err := client.conn.Write(data)
		if err != nil {
			return fmt.Errorf("write RFB message: %w", err)
		}

		if written == 0 {
			return io.ErrShortWrite
		}

		data = data[written:]
	}

	return nil
}

type keyTransition struct{ down, hold bool }

func (client *rfbClient) key(symbol uint32, down bool) error {
	return client.sendKey(symbol, keyTransition{down: down})
}

func (client *rfbClient) sendKey(
	symbol uint32,
	transition keyTransition,
) error {
	if client.readonly {
		return nil
	}

	msg := []byte{4, 0, 0, 0, 0, 0, 0, 0}
	if transition.down {
		msg[1] = 1
	}

	binary.BigEndian.PutUint32(msg[4:], symbol)

	return client.write(msg)
}

func (client *rfbClient) pointer(mask byte, column, row uint16) error {
	if client.readonly {
		return nil
	}

	msg := []byte{5, mask, 0, 0, 0, 0}
	binary.BigEndian.PutUint16(msg[2:], column)
	binary.BigEndian.PutUint16(msg[4:], row)

	return client.write(msg)
}

type framebufferRequest struct{ incremental bool }

func (client *rfbClient) request(incremental bool) error {
	return client.requestFrame(framebufferRequest{incremental: incremental})
}

func (client *rfbClient) requestFrame(request framebufferRequest) error {
	msg := []byte{3, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	if request.incremental {
		msg[1] = 1
	}

	width, height := client.frame.Rect.Dx(), client.frame.Rect.Dy()
	if width < 1 || width > math.MaxUint16 || height < 1 ||
		height > math.MaxUint16 {
		return errors.New("invalid framebuffer request dimensions")
	}

	binary.BigEndian.PutUint16(msg[6:], uint16(width))
	binary.BigEndian.PutUint16(msg[8:], uint16(height))

	return client.write(msg)
}

func (client *rfbClient) resize(width, height int) error {
	if width <= 0 || height <= 0 || width > 65535 || height > 65535 ||
		width > maxPixels/height {
		return fmt.Errorf(
			"framebuffer dimensions %dx%d exceed limit",
			width,
			height,
		)
	}

	frame := image.NewRGBA(image.Rect(0, 0, width, height))
	for i := 3; i < len(frame.Pix); i += 4 {
		frame.Pix[i] = 255
	}

	if client.frame != nil {
		draw.Draw(frame, frame.Rect, client.frame, image.Point{}, draw.Src)
	}

	client.frame = frame
	client.hasPixels = false

	return nil
}

func (client *rfbClient) stream(
	ctx context.Context,
	frames chan<- *image.RGBA,
) error {
	requestError := client.request(false)
	if requestError != nil {
		return requestError
	}

	for {
		var kind [1]byte

		_, readError := io.ReadFull(client.conn, kind[:])
		if readError != nil {
			return fmt.Errorf("read server message: %w", readError)
		}

		done, messageError := client.serverMessage(ctx, frames, kind[0])
		if done || messageError != nil {
			return messageError
		}
	}
}

func (client *rfbClient) serverMessage(
	ctx context.Context,
	frames chan<- *image.RGBA,
	kind byte,
) (bool, error) {
	switch kind {
	case 0:
		return client.publishUpdate(ctx, frames)

	case 1:
		return false, client.discardColorMap()

	case serverBellMessage:
		return false, nil

	case serverClipboardMessage:
		return false, client.discardClipboard()

	default:
		return false, fmt.Errorf("unsupported server message %d", kind)
	}
}

func (client *rfbClient) publishUpdate(
	ctx context.Context,
	frames chan<- *image.RGBA,
) (bool, error) {
	resized, err := client.update()
	if err != nil {
		return false, fmt.Errorf("framebuffer update: %w", err)
	}

	if client.singleFrame && (resized || !client.hasPixels) {
		return false, client.request(false)
	}

	frame := image.NewRGBA(client.frame.Rect)
	copy(frame.Pix, client.frame.Pix)

	select {
	case frames <- frame:
	case <-ctx.Done():
		return false, fmt.Errorf("publish framebuffer: %w", ctx.Err())
	}

	if client.singleFrame {
		return true, nil
	}

	return false, client.request(!resized)
}

func (client *rfbClient) discardColorMap() error {
	var header [5]byte

	_, readError := io.ReadFull(client.conn, header[:])
	if readError != nil {
		return fmt.Errorf("read color map: %w", readError)
	}

	first := int(binary.BigEndian.Uint16(header[1:3]))
	count := int(binary.BigEndian.Uint16(header[3:5]))

	if first+count > colorMapCapacity {
		return errors.New("invalid color map range")
	}

	_, discardError := io.CopyN(
		io.Discard,
		client.conn,
		int64(count*colorMapEntryBytes),
	)
	if discardError != nil {
		return fmt.Errorf("discard color map: %w", discardError)
	}

	return nil
}

func (client *rfbClient) discardClipboard() error {
	var padding [3]byte

	_, readError := io.ReadFull(client.conn, padding[:])
	if readError != nil {
		return fmt.Errorf("read clipboard: %w", readError)
	}

	err := discardText(client.conn)
	if err != nil {
		return fmt.Errorf("server clipboard: %w", err)
	}

	return nil
}

func (client *rfbClient) authenticate(password func() ([]byte, error)) error {
	var challenge [16]byte

	_, readFullError := io.ReadFull(client.conn, challenge[:])
	if readFullError != nil {
		return fmt.Errorf("read authentication challenge: %w", readFullError)
	}

	secret, err := password()
	if err != nil {
		clear(secret)

		if errors.Is(err, context.Canceled) {
			return context.Canceled
		}

		if errors.Is(err, context.DeadlineExceeded) {
			return context.DeadlineExceeded
		}

		return errors.New("password entry cancelled or unavailable")
	}

	response := challengeResponse(secret, challenge)
	clear(secret)

	return client.write(response[:])
}

func (client *rfbClient) authenticationResult(minor int) error {
	var status uint32

	readError := binary.Read(client.conn, binary.BigEndian, &status)
	if readError != nil {
		return fmt.Errorf("read authentication result: %w", readError)
	}

	if status != 0 {
		if minor == protocolMinorModern {
			err := discardText(client.conn)
			if err != nil {
				return fmt.Errorf(
					"authentication failed (invalid failure response): %w",
					err,
				)
			}
		}

		return errors.New(
			"authentication failed; check password and server policy",
		)
	}

	return nil
}

func legacyVersion(version [12]byte) bool {
	if string(version[:8]) != "RFB 003." || version[11] != '\n' {
		return false
	}

	for _, digit := range version[8:11] {
		if digit < '0' || digit > '9' {
			return false
		}
	}

	return true
}
