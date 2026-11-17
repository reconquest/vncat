package main

import (
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"math"
	"os"

	"golang.org/x/sys/unix"
	"golang.org/x/term"

	uv "github.com/charmbracelet/ultraviolet"
)

const (
	enterScreen = "\x1b[?1049h\x1b[?25s\x1b[?25l\x1b[2J\x1b[H"
	enterInput  = "\x1b[?1000s\x1b[?1002s\x1b[?1003s\x1b[?1006s" +
		"\x1b[?1016s\x1b[?1000l\x1b[?1002l\x1b[?1003l" +
		"\x1b[?1016$p\x1b[?1016l\x1b[?1006h\x1b[>15u\x1b[?u"
)

const (
	beginSync   = "\x1b[?2026h"
	endSync     = "\x1b[?2026l"
	leaveScreen = "\x1b\\" + endSync + "\x1b_Ga=d,d=I,i=1,q=2\x1b\\" +
		"\x1b_Ga=d,d=I,i=31,q=2\x1b\\\x1b[?25h\x1b[?25r\x1b[?1049l"
)

const (
	leaveInputEncoding = "\x1b[<u\x1b[?1003l\x1b[?1002l\x1b[?1000l" +
		"\x1b[?1016l\x1b[?1006l\x1b[?1016r\x1b[?1006r"
	restoreMouseTracking = "\x1b[?1003r\x1b[?1002r\x1b[?1000r"
	leaveInput           = leaveInputEncoding + restoreMouseTracking
)

const (
	querySize     = "\x1b[16t\x1b[14t"
	queryGraphics = "\x1b_Ga=q,i=31,f=24,s=1,v=1;AAAA\x1b\\"
)

type terminalSession struct {
	input               *os.File
	output              io.Writer
	state               *term.State
	snapshot            *snapshotPresentation
	tmux                *tmuxGraphics
	tmuxSupport         tmuxCapabilities
	tmuxActive          bool
	inputEnabled        bool
	mouseOriginalPixels bool
	inputDecoder        eventDecoder
	initialEvents       []uv.Event
}

func openTerminal(input *os.File, output io.Writer) (*terminalSession, error) {
	state, err := term.MakeRaw(int(input.Fd()))
	if err != nil {
		return nil, fmt.Errorf("enter raw terminal: %w", err)
	}

	session := &terminalSession{input: input, output: output, state: state}

	_, err = io.WriteString(output, enterScreen)
	if err != nil {
		return nil, errors.Join(err, session.close())
	}

	return session, nil
}

func (session *terminalSession) enableInput() error {
	queries := querySize + queryGraphics
	if session.tmux != nil {
		queries += session.tmux.queries()
	}

	if session.snapshot == nil {
		session.inputEnabled = true
		queries = enterInput + queries

		if session.tmux == nil {
			queries += "\x1b[>q"
		}
	}

	_, err := io.WriteString(session.output, queries)
	if err != nil {
		return fmt.Errorf("enable input: %w", err)
	}

	return nil
}

func (session *terminalSession) close() error {
	if session.state == nil {
		return nil
	}

	var outputError, graphicsError error

	if session.tmux != nil {
		graphicsError = session.tmux.close(session.output)
	}

	if session.inputEnabled {
		restore := leaveInput
		if session.mouseOriginalPixels {
			restore = leaveInputEncoding + "\x1b[?1016r" + restoreMouseTracking
		}

		_, outputError = io.WriteString(session.output, restore)
	}

	_, screenError := io.WriteString(session.output, leaveScreen)
	restoreError := term.Restore(int(session.input.Fd()), session.state)

	session.state = nil

	return errors.Join(graphicsError, outputError, screenError, restoreError)
}

func (session *terminalSession) render(
	frame *image.RGBA,
	size terminalSize,
	readonly bool,
	fps float64,
) (viewport, error) {
	if session.snapshot != nil {
		return viewport{}, nil
	}

	if session.tmuxActive {
		return session.tmux.render(
			session.output, frame, size,
			footerStatus{readonly: readonly, fps: fps},
		)
	}

	return render(session.output, frame, size, readonly, fps)
}

func readSize(fd int, previous terminalSize) (terminalSize, error) {
	size, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	if err != nil {
		return terminalSize{}, fmt.Errorf("read terminal size: %w", err)
	}

	previous.columns, previous.rows = int(size.Col), int(size.Row)
	previous.cellWidth, previous.cellHeight = 0, 0

	if size.Col > 0 && size.Row > 0 && size.Xpixel >= size.Col &&
		size.Ypixel >= size.Row {
		previous.cellWidth = int(size.Xpixel / size.Col)
		previous.cellHeight = int(size.Ypixel / size.Row)
	}

	return previous, nil
}

func pollInput(ctx context.Context, descriptor int, data []byte) (int, error) {
	err := ctx.Err()
	if err != nil {
		return 0, fmt.Errorf("poll input: %w", err)
	}

	if descriptor < 0 || descriptor > math.MaxInt32 {
		return 0, errors.New("invalid input descriptor")
	}

	const pollMilliseconds = 25

	fds := []unix.PollFd{{Fd: int32(descriptor), Events: unix.POLLIN}}
	count, err := unix.Poll(fds, pollMilliseconds)

	if errors.Is(err, unix.EINTR) {
		return 0, nil
	}

	if err != nil {
		return 0, fmt.Errorf("poll input: %w", err)
	}

	if count == 0 {
		return 0, nil
	}

	return pollResult(descriptor, data, fds[0].Revents)
}

func pollResult(fd int, data []byte, events int16) (int, error) {
	if events&unix.POLLIN != 0 {
		return readInput(fd, data)
	}

	if events&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 {
		return 0, io.EOF
	}

	return 0, nil
}

func readInput(fd int, data []byte) (int, error) {
	count, err := unix.Read(fd, data)
	if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
		return 0, nil
	}

	if err != nil {
		return count, fmt.Errorf("read input: %w", err)
	}

	if count == 0 {
		return 0, io.EOF
	}

	return count, nil
}
