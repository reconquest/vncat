package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
	"unicode/utf8"

	uv "github.com/charmbracelet/ultraviolet"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

const enterScreen = "\x1b[?1049h\x1b[?25s\x1b[?25l\x1b[2J\x1b[H"
const enterInput = "\x1b[?1000s\x1b[?1002s\x1b[?1003s\x1b[?1006s" +
	"\x1b[?1000l\x1b[?1002l\x1b[?1003h\x1b[?1006h\x1b[>15u\x1b[?u"
const beginSync = "\x1b[?2026h"
const endSync = "\x1b[?2026l"
const leaveScreen = "\x1b\\" + endSync + "\x1b_Ga=d,d=I,i=1,q=2\x1b\\" +
	"\x1b_Ga=d,d=I,i=31,q=2\x1b\\\x1b[?25h\x1b[?25r\x1b[?1049l"
const leaveInput = "\x1b[<u\x1b[?1006l\x1b[?1003l\x1b[?1002l\x1b[?1000l" +
	"\x1b[?1006r\x1b[?1003r\x1b[?1002r\x1b[?1000r"
const querySize = "\x1b[16t\x1b[14t"
const queryGraphics = "\x1b_Ga=q,i=31,f=24,s=1,v=1;AAAA\x1b\\"

type terminalSession struct {
	input        *os.File
	output       io.Writer
	state        *term.State
	inputEnabled bool
}

func openTerminal(input *os.File, output io.Writer) (*terminalSession, error) {
	state, err := term.MakeRaw(int(input.Fd()))
	if err != nil {
		return nil, fmt.Errorf("enter raw terminal: %w", err)
	}
	session := &terminalSession{input: input, output: output, state: state}
	if _, err := io.WriteString(output, enterScreen); err != nil {
		return nil, errors.Join(err, session.close())
	}
	return session, nil
}

func (t *terminalSession) enableInput() error {
	t.inputEnabled = true
	_, err := io.WriteString(t.output, enterInput+querySize+queryGraphics)
	return err
}

func (t *terminalSession) close() error {
	if t.state == nil {
		return nil
	}
	var outputError error
	if t.inputEnabled {
		_, outputError = io.WriteString(t.output, leaveInput)
	}
	_, screenError := io.WriteString(t.output, leaveScreen)
	restoreError := term.Restore(int(t.input.Fd()), t.state)
	t.state = nil
	return errors.Join(outputError, screenError, restoreError)
}

func readSize(fd int, previous terminalSize) (terminalSize, error) {
	size, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	if err != nil {
		return terminalSize{}, fmt.Errorf("read terminal size: %w", err)
	}
	previous.columns, previous.rows = int(size.Col), int(size.Row)
	if size.Col > 0 && size.Row > 0 && size.Xpixel >= size.Col && size.Ypixel >= size.Row {
		previous.cellWidth = int(size.Xpixel / size.Col)
		previous.cellHeight = int(size.Ypixel / size.Row)
	}
	return previous, nil
}

func pollInput(ctx context.Context, fd int, data []byte) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, 25)
	if errors.Is(err, unix.EINTR) {
		return 0, nil
	}
	if err != nil || n == 0 {
		return 0, err
	}
	if fds[0].Revents&unix.POLLIN != 0 {
		n, err := unix.Read(fd, data)
		if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
			return 0, nil
		}
		if n == 0 && err == nil {
			return 0, io.EOF
		}
		return n, err
	}
	if fds[0].Revents&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 {
		return 0, io.EOF
	}
	return 0, nil
}

func readPassword(ctx context.Context, fd int, out io.Writer) ([]byte, error) {
	if _, err := io.WriteString(out, "\x1b[HVNC password (first 8 bytes used): "); err != nil {
		return nil, err
	}
	secret := make([]byte, 0, 64)
	defer func() { io.WriteString(out, "\x1b[2J\x1b[H") }()
	var data [1]byte
	for {
		n, err := pollInput(ctx, fd, data[:])
		if err != nil {
			clear(secret)
			return nil, err
		}
		if n == 0 {
			continue
		}
		switch data[0] {
		case '\r', '\n':
			return secret, nil
		case 3, 29:
			clear(secret)
			return nil, context.Canceled
		case 8, 127:
			if len(secret) > 0 {
				_, width := utf8.DecodeLastRune(secret)
				clear(secret[len(secret)-width:])
				secret = secret[:len(secret)-width]
			}
		default:
			if len(secret) >= 1024 {
				clear(secret)
				return nil, errors.New("password too long")
			}
			secret = append(secret, data[0])
		}
	}
}

type eventDecoder struct {
	decoder uv.EventDecoder
	pending []byte
}

func (d *eventDecoder) feed(data []byte, expired bool) ([]uv.Event, error) {
	d.pending = append(d.pending, data...)
	events := make([]uv.Event, 0)
	for len(d.pending) > 0 {
		size := sequenceLength(d.pending)
		if size == 0 {
			if len(d.pending) > 4096 {
				return nil, errors.New("terminal input sequence exceeds limit")
			}
			if !expired {
				break
			}
			if len(d.pending) > 2 || d.pending[0] != 27 {
				d.pending = nil
				break
			}
			size = len(d.pending)
		}
		if size > 4096 {
			return nil, errors.New("terminal input sequence exceeds limit")
		}
		sequence := d.pending[:size]
		n, event := d.decoder.Decode(sequence)
		if key, ok := event.(uv.KeyPressEvent); ok {
			escapeKey := len(sequence) >= 3 && sequence[0] == 27 &&
				sequence[1] == '['
			if !escapeKey {
				event = legacyKeyEvent(key)
			}
		}
		if n > 0 && event != nil {
			if multi, ok := event.(uv.MultiEvent); ok {
				events = append(events, multi...)
			} else {
				events = append(events, event)
			}
		}
		d.pending = d.pending[size:]
	}
	return events, nil
}

func sequenceLength(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	if data[0] != 27 {
		if !utf8.FullRune(data) {
			return 0
		}
		_, size := utf8.DecodeRune(data)
		return size
	}
	if len(data) < 2 {
		return 0
	}
	switch data[1] {
	case '[', 'O':
		for i := 2; i < len(data); i++ {
			if data[i] >= 0x40 && data[i] <= 0x7e {
				if data[1] == '[' && i == 2 && data[i] == 'M' {
					if len(data) < 6 {
						return 0
					}
					return 6
				}
				return i + 1
			}
		}
		return 0
	case ']', 'P', '_', '^', 'X':
		if end := bytes.Index(data[2:], []byte{27, '\\'}); end >= 0 {
			return end + 4
		}
		if data[1] == ']' {
			if end := bytes.IndexByte(data[2:], 7); end >= 0 {
				return end + 3
			}
		}
		return 0
	default:
		if !utf8.FullRune(data[1:]) {
			return 0
		}
		_, size := utf8.DecodeRune(data[1:])
		return 1 + size
	}
}

func streamInput(ctx context.Context, fd int, events chan<- uv.Event) error {
	var decoder eventDecoder
	var data [1024]byte
	last := time.Now()
	for {
		n, err := pollInput(ctx, fd, data[:])
		if err != nil {
			return err
		}
		if n > 0 {
			last = time.Now()
		}
		decoded, err := decoder.feed(data[:n], time.Since(last) > 75*time.Millisecond)
		if err != nil {
			return err
		}
		for _, event := range decoded {
			select {
			case events <- event:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}
