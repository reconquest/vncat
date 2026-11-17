package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	uv "github.com/charmbracelet/ultraviolet"
)

const (
	mouseReplyTimeout = 250 * time.Millisecond
	startupEventLimit = 256
	cellMouseTracking = "\x1b[?1016l\x1b[?1006h\x1b[?1003h"
)

func (session *terminalSession) negotiateMouse(ctx context.Context) error {
	if !session.inputEnabled {
		return nil
	}

	pixels, err := session.selectMouseEncoding(ctx)
	if err != nil {
		return err
	}

	session.inputDecoder.mousePixels = pixels

	tracking := cellMouseTracking

	if pixels {
		tracking = "\x1b[?1003h"
	}

	_, err = io.WriteString(session.output, tracking)
	if err != nil {
		return fmt.Errorf("enable mouse tracking: %w", err)
	}

	return nil
}

func (session *terminalSession) selectMouseEncoding(
	ctx context.Context,
) (bool, error) {
	if session.tmux != nil {
		return false, nil
	}

	ghostty, err := session.identifyMouseTerminal(ctx)
	if err != nil || !ghostty {
		return false, err
	}

	return session.requestPixelMouse(ctx)
}

func (session *terminalSession) identifyMouseTerminal(
	ctx context.Context,
) (bool, error) {
	ghostty := false
	err := session.awaitMouseReply(ctx, func(event uv.Event) bool {
		switch event := event.(type) {
		case uv.ModeReportEvent:
			if event.Mode == ansi.ModeMouseExtSgrPixel {
				session.mouseOriginalPixels = event.Value.IsSet()
			}

		case uv.TerminalVersionEvent:
			version, recognized := strings.CutPrefix(event.Name, "ghostty ")

			ghostty = recognized &&
				terminalVersionAtLeast(version, [3]int{1, 4, 0})

			return true

		default:
		}

		return false
	})

	return ghostty, err
}

func (session *terminalSession) requestPixelMouse(
	ctx context.Context,
) (bool, error) {
	_, err := io.WriteString(session.output,
		ansi.SetModeMouseExtSgrPixel+ansi.RequestModeMouseExtSgrPixel)
	if err != nil {
		return false, fmt.Errorf("request pixel mouse: %w", err)
	}

	pixels := false

	err = session.awaitMouseReply(ctx, func(event uv.Event) bool {
		report, ok := event.(uv.ModeReportEvent)
		if !ok || report.Mode != ansi.ModeMouseExtSgrPixel {
			return false
		}

		pixels = report.Value.IsSet()

		return true
	})

	return pixels, err
}

func (session *terminalSession) awaitMouseReply(
	parent context.Context,
	accept func(uv.Event) bool,
) error {
	ctx, cancel := context.WithTimeout(parent, mouseReplyTimeout)
	defer cancel()

	err := session.readMouseReplies(ctx, accept)
	if errors.Is(err, context.DeadlineExceeded) && parent.Err() == nil {
		return nil
	}

	return err
}

func (session *terminalSession) readMouseReplies(
	ctx context.Context,
	accept func(uv.Event) bool,
) error {
	var data [1024]byte

	last := time.Now()

	for {
		events, err := session.inputDecoder.readEvents(
			ctx, int(session.input.Fd()), data[:], &last,
		)
		if err != nil {
			return err
		}

		matched, err := session.collectInitialEvents(events, accept)
		if err != nil || matched {
			return err
		}
	}
}

func (session *terminalSession) collectInitialEvents(
	events []uv.Event,
	accept func(uv.Event) bool,
) (bool, error) {
	matched := false

	for _, event := range events {
		if !matched {
			matched = accept(event)
		}

		switch event.(type) {
		case uv.MouseEvent, pixelMouseEvent,
			uv.ModeReportEvent, uv.TerminalVersionEvent:
			continue

		default:
		}

		if len(session.initialEvents) >= startupEventLimit {
			return false, errors.New("too many events during mouse negotiation")
		}

		session.initialEvents = append(session.initialEvents, event)
	}

	return matched, nil
}

func (session *terminalSession) streamInput(
	ctx context.Context,
	events chan<- uv.Event,
) error {
	err := publishEvents(ctx, events, session.initialEvents)
	if err != nil {
		return err
	}

	session.initialEvents = nil

	return session.inputDecoder.streamInput(
		ctx,
		int(session.input.Fd()),
		events,
	)
}
