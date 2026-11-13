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
	"math/bits"
	"net"
	"sync"
	"time"
)

const maxPixels = 16 * 1024 * 1024
const maxText = 1024 * 1024

var wireOrder = binary.BigEndian

type rfbClient struct {
	conn     net.Conn
	writes   sync.Mutex
	readonly bool
	frame    *image.RGBA
}

func (c *rfbClient) handshake(ctx context.Context, password func() ([]byte, error)) error {
	if err := c.conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return err
	}
	stopped := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { c.conn.SetDeadline(time.Now()); close(stopped) })
	defer func() {
		if !stop() {
			<-stopped
		}
		c.conn.SetDeadline(time.Time{})
	}()
	if err := c.negotiate(password); err != nil {
		return fmt.Errorf("RFB handshake: %w", err)
	}
	return nil
}

func (c *rfbClient) negotiate(password func() ([]byte, error)) error {
	var version [12]byte
	if _, err := io.ReadFull(c.conn, version[:]); err != nil {
		return fmt.Errorf("read version: %w", err)
	}
	minor := 0
	switch string(version[:]) {
	case "RFB 003.003\n":
		minor = 3
	case "RFB 003.007\n":
		minor = 7
	case "RFB 003.008\n":
		minor = 8
	default:
		return fmt.Errorf("unsupported protocol version %q", version)
	}
	if err := c.write(version[:]); err != nil {
		return err
	}
	security, err := c.security(minor)
	if err != nil {
		return err
	}
	if security == 2 {
		var challenge [16]byte
		if _, err := io.ReadFull(c.conn, challenge[:]); err != nil {
			return fmt.Errorf("read authentication challenge: %w", err)
		}
		secret, err := password()
		if err != nil {
			return errors.New("password entry cancelled or unavailable")
		}
		response := challengeResponse(secret, challenge)
		clear(secret)
		if err := c.write(response[:]); err != nil {
			return err
		}
	}
	if security == 2 || minor == 8 {
		var status uint32
		if err := binary.Read(c.conn, wireOrder, &status); err != nil {
			return fmt.Errorf("read authentication result: %w", err)
		}
		if status != 0 {
			if minor == 8 {
				if err := discardText(c.conn); err != nil {
					return fmt.Errorf("authentication failed (invalid failure response): %w", err)
				}
			}
			return errors.New("authentication failed; check password and server policy")
		}
	}
	if err := c.write([]byte{1}); err != nil {
		return err
	}
	var init [24]byte
	if _, err := io.ReadFull(c.conn, init[:]); err != nil {
		return fmt.Errorf("read server initialization: %w", err)
	}
	width, height := int(wireOrder.Uint16(init[:2])), int(wireOrder.Uint16(init[2:4]))
	if err := c.resize(width, height); err != nil {
		return err
	}
	if err := discardSized(c.conn, wireOrder.Uint32(init[20:24])); err != nil {
		return fmt.Errorf("desktop name: %w", err)
	}
	format := []byte{0, 0, 0, 0, 32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 16, 8, 0, 0, 0, 0}
	if err := c.write(format); err != nil {
		return err
	}
	encodings := []int32{1, 5, 0, -223, -224}
	msg := make([]byte, 4+4*len(encodings))
	msg[0] = 2
	wireOrder.PutUint16(msg[2:4], uint16(len(encodings)))
	for i, encoding := range encodings {
		wireOrder.PutUint32(msg[4+i*4:], uint32(encoding))
	}
	return c.write(msg)
}

func (c *rfbClient) security(minor int) (uint32, error) {
	if minor == 3 {
		var security uint32
		if err := binary.Read(c.conn, wireOrder, &security); err != nil {
			return 0, fmt.Errorf("read security type: %w", err)
		}
		if security == 0 {
			return 0, c.rejected()
		}
		if security != 1 && security != 2 {
			return 0, fmt.Errorf("unsupported security type %d (supported: None, VNC password)", security)
		}
		return security, nil
	}
	var count [1]byte
	if _, err := io.ReadFull(c.conn, count[:]); err != nil {
		return 0, fmt.Errorf("read security types: %w", err)
	}
	if count[0] == 0 {
		return 0, c.rejected()
	}
	types := make([]byte, int(count[0]))
	if _, err := io.ReadFull(c.conn, types); err != nil {
		return 0, err
	}
	for _, preferred := range []byte{2, 1} {
		for _, offered := range types {
			if preferred == offered {
				return uint32(preferred), c.write([]byte{preferred})
			}
		}
	}
	return 0, fmt.Errorf("unsupported security types %v (supported: None, VNC password)", types)
}

func (c *rfbClient) rejected() error {
	if err := discardText(c.conn); err != nil {
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

func discardText(r io.Reader) error {
	var size uint32
	if err := binary.Read(r, wireOrder, &size); err != nil {
		return err
	}
	return discardSized(r, size)
}

func discardSized(r io.Reader, size uint32) error {
	if size > maxText {
		return fmt.Errorf("text length %d exceeds limit", size)
	}
	_, err := io.CopyN(io.Discard, r, int64(size))
	return err
}

func (c *rfbClient) write(data []byte) error {
	c.writes.Lock()
	defer c.writes.Unlock()
	if err := c.conn.SetWriteDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return err
	}
	for len(data) > 0 {
		n, err := c.conn.Write(data)
		if err != nil {
			return fmt.Errorf("write RFB message: %w", err)
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func (c *rfbClient) key(symbol uint32, down bool) error {
	if c.readonly {
		return nil
	}
	msg := []byte{4, 0, 0, 0, 0, 0, 0, 0}
	if down {
		msg[1] = 1
	}
	wireOrder.PutUint32(msg[4:], symbol)
	return c.write(msg)
}

func (c *rfbClient) pointer(mask byte, x, y uint16) error {
	if c.readonly {
		return nil
	}
	msg := []byte{5, mask, 0, 0, 0, 0}
	wireOrder.PutUint16(msg[2:], x)
	wireOrder.PutUint16(msg[4:], y)
	return c.write(msg)
}

func (c *rfbClient) request(incremental bool) error {
	msg := []byte{3, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	if incremental {
		msg[1] = 1
	}
	wireOrder.PutUint16(msg[6:], uint16(c.frame.Rect.Dx()))
	wireOrder.PutUint16(msg[8:], uint16(c.frame.Rect.Dy()))
	return c.write(msg)
}

func (c *rfbClient) resize(width, height int) error {
	if width <= 0 || height <= 0 || width > 65535 || height > 65535 || width > maxPixels/height {
		return fmt.Errorf("framebuffer dimensions %dx%d exceed limit", width, height)
	}
	frame := image.NewRGBA(image.Rect(0, 0, width, height))
	for i := 3; i < len(frame.Pix); i += 4 {
		frame.Pix[i] = 255
	}
	if c.frame != nil {
		draw.Draw(frame, frame.Rect, c.frame, image.Point{}, draw.Src)
	}
	c.frame = frame
	return nil
}

func (c *rfbClient) stream(ctx context.Context, frames chan<- *image.RGBA) error {
	if err := c.request(false); err != nil {
		return err
	}
	for {
		var kind [1]byte
		if _, err := io.ReadFull(c.conn, kind[:]); err != nil {
			return fmt.Errorf("read server message: %w", err)
		}
		switch kind[0] {
		case 0:
			resized, err := c.update()
			if err != nil {
				return fmt.Errorf("framebuffer update: %w", err)
			}
			frame := image.NewRGBA(c.frame.Rect)
			copy(frame.Pix, c.frame.Pix)
			select {
			case frames <- frame:
			case <-ctx.Done():
				return ctx.Err()
			}
			if err := c.request(!resized); err != nil {
				return err
			}
		case 1:
			var header [5]byte
			if _, err := io.ReadFull(c.conn, header[:]); err != nil {
				return err
			}
			first, count := int(wireOrder.Uint16(header[1:3])), int(wireOrder.Uint16(header[3:5]))
			if first+count > 65536 {
				return errors.New("invalid color map range")
			}
			if _, err := io.CopyN(io.Discard, c.conn, int64(count*6)); err != nil {
				return err
			}
		case 2:
		case 3:
			var padding [3]byte
			if _, err := io.ReadFull(c.conn, padding[:]); err != nil {
				return err
			}
			if err := discardText(c.conn); err != nil {
				return fmt.Errorf("server clipboard: %w", err)
			}
		default:
			return fmt.Errorf("unsupported server message %d", kind[0])
		}
	}
}
