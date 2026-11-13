package main

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"

	uv "github.com/charmbracelet/ultraviolet"
)

type tmuxCapabilities struct {
	graphics     bool
	placeholders bool
}

type graphicsFeaturesEvent struct {
	id           int
	placeholders bool
}

func (graphics *tmuxGraphics) queries() string {
	base := fmt.Sprintf(
		"\x1b_Ga=q,i=%d,f=24,s=1,v=1;AAAA\x1b\\", graphics.queryID,
	)
	features := fmt.Sprintf(
		"\x1b_Ga=q,i=%d,e=T,U=1\x1b\\", graphics.featureID,
	)

	var queries strings.Builder

	_, _ = queries.WriteString(
		ansi.TmuxPassthrough(base) + ansi.TmuxPassthrough(features),
	)

	const versionAttempts = 2

	for range versionAttempts {
		_, _ = queries.WriteString(
			ansi.TmuxPassthrough(ansi.RequestNameVersion),
		)
	}

	return queries.String()
}

func (session *terminalSession) acceptGraphics(
	event *uv.KittyGraphicsEvent,
) (bool, error) {
	imageID := event.Options.ID

	if session.tmux != nil {
		switch imageID {
		case session.tmux.queryID:
			session.tmuxSupport.graphics = string(event.Payload) == "OK"

			return session.enableTmux(), nil

		case session.tmux.featureID:
			return false, nil
		}
	}

	if imageID != graphicsQueryID && !session.ownsImage(imageID) {
		return false, nil
	}

	if string(event.Payload) != "OK" {
		return false, errors.New("terminal rejected Kitty graphics operation")
	}

	return imageID == graphicsQueryID, nil
}

func (session *terminalSession) ownsImage(id int) bool {
	if session.tmuxActive {
		return slices.Contains(session.tmux.imageIDs[:], id)
	}

	return id == 1
}

func (session *terminalSession) acceptGraphicsFeatures(
	event graphicsFeaturesEvent,
) {
	if session.tmux != nil && event.id == session.tmux.featureID &&
		event.placeholders {
		session.tmuxSupport.placeholders = true
	}
}

func (session *terminalSession) acceptTerminalVersion(name string) {
	if session.tmux != nil && placeholderTerminal(name) {
		session.tmuxSupport.placeholders = true
	}
}

func (session *terminalSession) enableTmux() bool {
	if session.tmux == nil || !session.tmuxSupport.graphics ||
		!session.tmuxSupport.placeholders {
		return false
	}

	session.tmuxActive = true

	return true
}

func (session *terminalSession) graphicsUnavailable() error {
	if session.tmux == nil {
		return errors.New("terminal did not confirm Kitty graphics support")
	}

	if session.tmuxSupport.graphics {
		return errors.New(
			"terminal did not confirm Unicode placeholders; " +
				"tmux requires Ghostty 1.0+, Kitty 0.28+, " +
				"or a terminal supporting Kitty feature queries",
		)
	}

	return errors.New(
		"terminal did not confirm Kitty graphics through tmux; " +
			"enable passthrough with: tmux set -g allow-passthrough on",
	)
}

func placeholderTerminal(name string) bool {
	switch {
	case strings.HasPrefix(name, "ghostty "):
		return terminalVersionAtLeast(
			strings.TrimPrefix(name, "ghostty "), [3]int{1, 0, 0},
		)

	case strings.HasPrefix(name, "kitty(") && strings.HasSuffix(name, ")"):
		return terminalVersionAtLeast(
			strings.TrimSuffix(strings.TrimPrefix(name, "kitty("), ")"),
			[3]int{0, 28, 0},
		)

	default:
		return false
	}
}

func terminalVersionAtLeast(version string, minimum [3]int) bool {
	core, _, _ := strings.Cut(version, "+")

	core, _, _ = strings.Cut(core, "-")

	parts := strings.Split(core, ".")

	var parsed [3]int

	if len(parts) != len(parsed) {
		return false
	}

	for index, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 {
			return false
		}

		parsed[index] = value
	}

	return slices.Compare(parsed[:], minimum[:]) >= 0
}

func captureGraphicsFeatures(sequence []byte, event *decodedEvent) {
	graphics, ok := event.value.(uv.KittyGraphicsEvent)
	if !ok {
		return
	}

	control, ok := bytes.CutPrefix(sequence, []byte("\x1b_G"))
	if !ok {
		return
	}

	header, _, _ := bytes.Cut(control, []byte(";"))
	for field := range bytes.SplitSeq(header, []byte(",")) {
		if bytes.Equal(field, []byte("e=T")) {
			event.value = graphicsFeaturesEvent{
				id: graphics.Options.ID,
				placeholders: graphics.Options.VirtualPlacement &&
					string(graphics.Payload) == "OK",
			}

			return
		}
	}
}
