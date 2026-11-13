package main

import (
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"os"
	"sync"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
)

type desktopSession struct {
	terminal         *terminalSession
	input            inputState
	size             terminalSize
	fps              frameRate
	frame            *image.RGBA
	graphicsReady    bool
	cellSizeReported bool
	dirty            bool
	frames           chan *image.RGBA
	events           chan uv.Event
	networkDone      chan error
	inputDone        chan error
	resizes          <-chan os.Signal
	workers          sync.WaitGroup
}

type sessionTimers struct {
	status       *time.Timer
	refresh      *time.Ticker
	capabilities *time.Timer
}

func serve(
	parent context.Context,
	client *rfbClient,
	terminal *terminalSession,
	resizes <-chan os.Signal,
) error {
	ctx, cancel := context.WithCancel(parent)
	session := desktopSession{
		terminal: terminal,
		input:    inputState{client: client},
		fps:      frameRate{since: time.Now()},
		frames: make(
			chan *image.RGBA,
			1,
		),
		events: make(chan uv.Event, inputQueueCapacity),
		networkDone: make(
			chan error,
			1,
		),
		inputDone: make(chan error, 1),
		resizes:   resizes,
	}

	session.workers.Go(
		func() { session.networkDone <- client.stream(ctx, session.frames) },
	)
	session.workers.Go(func() {
		session.inputDone <- streamInput(
			ctx, int(terminal.input.Fd()), session.events,
		)
	})

	result := session.loop(ctx)
	releaseError := session.input.release()

	cancel()

	_ = client.conn.Close()

	session.workers.Wait()

	return errors.Join(result, releaseError)
}

func (session *desktopSession) loop(ctx context.Context) error {
	size, sizeError := readSize(
		int(session.terminal.input.Fd()),
		terminalSize{},
	)
	if sizeError != nil {
		return sizeError
	}

	session.size = size

	_, renderError := session.terminal.render(
		nil,
		size,
		session.input.client.readonly,
		session.fps.value,
	)
	if renderError != nil {
		return renderError
	}

	timers := sessionTimers{
		status: time.NewTimer(
			time.Second,
		),
		refresh:      time.NewTicker(time.Second / refreshFrequency),
		capabilities: time.NewTimer(capabilitySeconds * time.Second),
	}

	defer timers.status.Stop()
	defer timers.refresh.Stop()
	defer timers.capabilities.Stop()

	for {
		quit, err := session.step(ctx, timers)
		if quit || err != nil || session.terminal.snapshot.ready() {
			return err
		}

		session.input.view.cellWidth = session.size.cellWidth
		session.input.view.cellHeight = session.size.cellHeight
	}
}

func (session *desktopSession) step(
	ctx context.Context,
	timers sessionTimers,
) (bool, error) {
	select {
	case <-ctx.Done():
		return true, nil

	case err := <-session.networkDone:
		session.networkDone = nil

		return err != nil, readerResult(err)

	case err := <-session.inputDone:
		return true, terminalReaderResult(err)

	case <-timers.capabilities.C:
		return false, session.checkCapabilities()

	case <-session.resizes:
		return false, session.resize(timers.capabilities)

	case frame := <-session.frames:
		session.frame, session.dirty = frame, true

		return false, nil

	case event := <-session.events:
		return session.handle(event)

	case <-timers.status.C:
		err := session.refreshFooter()

		timers.status.Reset(time.Second)

		return false, err

	case <-timers.refresh.C:
		return false, session.refreshImage()
	}
}

func readerResult(err error) error {
	if errors.Is(err, context.Canceled) {
		return nil
	}

	return err
}

func terminalReaderResult(err error) error {
	if errors.Is(err, context.Canceled) {
		return nil
	}

	return fmt.Errorf("read terminal input: %w", err)
}

func (session *desktopSession) checkCapabilities() error {
	if !session.graphicsReady {
		return session.terminal.graphicsUnavailable()
	}

	if session.size.cellWidth <= 0 || session.size.cellHeight <= 0 {
		return errors.New("terminal did not report pixel dimensions")
	}

	return nil
}

func (session *desktopSession) resize(capabilities *time.Timer) error {
	session.cellSizeReported = false

	size, sizeError := readSize(int(session.terminal.input.Fd()), session.size)
	if sizeError != nil {
		return sizeError
	}

	session.size = size
	if size.cellWidth <= 0 || size.cellHeight <= 0 {
		capabilities.Reset(capabilitySeconds * time.Second)
	}

	view, renderError := session.terminal.render(
		nil,
		size,
		session.input.client.readonly,
		session.fps.value,
	)
	if renderError != nil {
		return renderError
	}

	session.input.view = view

	_, queryError := io.WriteString(session.terminal.output, querySize)
	if queryError != nil {
		return fmt.Errorf("query terminal size: %w", queryError)
	}

	session.dirty = true

	return nil
}

func (session *desktopSession) handle(event uv.Event) (bool, error) {
	switch event := event.(type) {
	case uv.KittyGraphicsEvent:
		return false, session.graphics(&event)

	case graphicsFeaturesEvent:
		session.terminal.acceptGraphicsFeatures(event)

		session.graphicsReady = session.graphicsReady ||
			session.terminal.enableTmux()

		return false, nil

	case uv.TerminalVersionEvent:
		session.terminal.acceptTerminalVersion(event.Name)

		session.graphicsReady = session.graphicsReady ||
			session.terminal.enableTmux()

		return false, nil

	case uv.CellSizeEvent:
		session.cellSize(event)

		return false, nil

	case uv.PixelSizeEvent:
		session.pixelSize(event)

		return false, nil

	default:
		quit, err := session.input.handle(event)
		if err != nil {
			return false, fmt.Errorf("forward remote input: %w", err)
		}

		return quit, nil
	}
}

func (session *desktopSession) graphics(event *uv.KittyGraphicsEvent) error {
	ready, err := session.terminal.acceptGraphics(event)
	if ready {
		session.graphicsReady = true
	}

	return err
}

func (session *desktopSession) cellSize(event uv.CellSizeEvent) {
	if event.Width <= 0 || event.Height <= 0 || event.Width > 1024 ||
		event.Height > 1024 {
		return
	}

	session.size.cellWidth, session.size.cellHeight = event.Width, event.Height
	session.cellSizeReported = true
	session.dirty = true
}

func (session *desktopSession) pixelSize(event uv.PixelSizeEvent) {
	size := session.size
	if session.cellSizeReported || size.columns <= 0 || size.rows <= 0 ||
		event.Width < size.columns ||
		event.Height < size.rows ||
		event.Width > 65535 ||
		event.Height > 65535 {
		return
	}

	session.size.cellWidth = event.Width / size.columns
	session.size.cellHeight = event.Height / size.rows
	session.dirty = true
}

func (session *desktopSession) refreshFooter() error {
	if session.terminal.snapshot != nil {
		return nil
	}

	session.fps.sample(time.Now())

	if session.terminal.tmuxActive {
		session.dirty = true

		return session.refreshImage()
	}

	return refreshStatus(
		session.terminal.output,
		session.size,
		session.input.client.readonly,
		session.fps.value,
	)
}

func (session *desktopSession) refreshImage() error {
	if !session.dirty || session.frame == nil || !session.graphicsReady ||
		!session.size.ready() {
		return nil
	}

	if session.terminal.snapshot != nil {
		return session.terminal.prepareSnapshot(session.frame, session.size)
	}

	view, err := session.terminal.render(
		session.frame,
		session.size,
		session.input.client.readonly,
		session.fps.value,
	)
	if err != nil {
		return err
	}

	if view.width > 0 && view.height > 0 {
		session.fps.frames++
	}

	session.input.view = view
	session.dirty = false

	return nil
}
