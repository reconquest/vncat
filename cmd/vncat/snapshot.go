package main

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"io"

	"github.com/charmbracelet/x/ansi/kitty"
)

type snapshotPresentation struct {
	upload []byte
	pane   []byte
}

func (session *terminalSession) prepareSnapshot(
	frame *image.RGBA,
	size terminalSize,
) error {
	var upload, pane bytes.Buffer

	_, _ = pane.WriteString(beginSync + "\x1b[2J\x1b[H")

	var err error

	if session.tmuxActive {
		_, err = prepareTmuxImage(
			&upload,
			&pane,
			frame,
			size,
			session.tmux.imageIDs[0],
			graphicsQuiet,
		)
	} else {
		err = prepareSnapshotImage(&pane, frame, size)
	}

	if err != nil {
		return err
	}

	_, _ = fmt.Fprintf(&pane, "\x1b[0m\x1b[%d;1H\x1b[2K", size.rows)

	session.snapshot.upload = upload.Bytes()
	session.snapshot.pane = pane.Bytes()

	return nil
}

func prepareSnapshotImage(
	output io.Writer,
	frame *image.RGBA,
	size terminalSize,
) error {
	view, err := fit(frame.Rect, size)
	if err != nil {
		return err
	}

	return encodeFrame(
		output,
		frame,
		view,
		&kitty.Options{Quiet: graphicsQuiet},
	)
}

func (snapshot *snapshotPresentation) ready() bool {
	return snapshot != nil && len(snapshot.pane) > 0
}

func (snapshot *snapshotPresentation) present(writer io.Writer) error {
	if !snapshot.ready() {
		return nil
	}

	if len(snapshot.upload) > 0 {
		err := writeTmuxGraphics(writer, string(snapshot.upload))
		if err != nil {
			return errors.Join(err, recoverTmuxGraphics(writer))
		}
	}

	return writePresentation(writer, bytes.NewBuffer(snapshot.pane))
}
