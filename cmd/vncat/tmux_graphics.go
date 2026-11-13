package main

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"image"
	"io"
	"math"
	"slices"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"
	"golang.org/x/image/draw"
)

const (
	tmuxDiacriticCount  = 297
	tmuxImageRedShift   = 16
	tmuxImageGreenShift = 8
)

type tmuxGraphics struct {
	imageIDs  [2]int
	queryID   int
	featureID int
	next      int
	failed    bool
}

func newTmuxGraphics() (*tmuxGraphics, error) {
	var ids [4]int

	for index := range ids {
		id, err := newTmuxImageID(ids[:index])
		if err != nil {
			return nil, err
		}

		ids[index] = id
	}

	return &tmuxGraphics{
		imageIDs:  [2]int{ids[0], ids[1]},
		queryID:   ids[2],
		featureID: ids[3],
	}, nil
}

func (graphics *tmuxGraphics) close(writer io.Writer) error {
	graphics.failed = true

	result := recoverTmuxGraphics(writer)

	for _, id := range []int{
		graphics.imageIDs[0], graphics.imageIDs[1],
		graphics.queryID, graphics.featureID,
	} {
		err := writeTmuxGraphics(writer, tmuxDeleteImage(id))
		if err != nil {
			result = errors.Join(result, err, recoverTmuxGraphics(writer))
		}
	}

	return result
}

func (graphics *tmuxGraphics) render(
	writer io.Writer,
	frame *image.RGBA,
	size terminalSize,
	status footerStatus,
) (viewport, error) {
	if graphics.failed {
		return viewport{}, errors.New("tmux graphics output is unavailable")
	}

	var upload, pane bytes.Buffer

	view, err := prepareTmuxPresentation(
		&upload, &pane, frame, size, status, graphics.imageIDs[graphics.next],
	)
	if err != nil {
		return viewport{}, err
	}

	for _, output := range []*bytes.Buffer{&upload, &pane} {
		err = writeTmuxGraphics(writer, output.String())
		if err != nil {
			graphics.failed = true

			return viewport{}, errors.Join(err, recoverTmuxGraphics(writer))
		}
	}

	if view.width > 0 {
		graphics.next = 1 - graphics.next
	}

	return view, nil
}

func prepareTmuxPresentation(
	upload, pane *bytes.Buffer,
	frame *image.RGBA,
	size terminalSize,
	status footerStatus,
	imageID int,
) (viewport, error) {
	var view viewport

	_, _ = pane.WriteString(beginSync + "\x1b[0m\x1b[2J\x1b[H")

	if frame != nil && size.rows >= 2 && size.columns >= 1 {
		prepared, err := prepareTmuxImage(
			upload, pane, frame, size, imageID, 1,
		)
		if err != nil {
			return viewport{}, err
		}

		view = prepared
	}

	_, _ = pane.WriteString("\x1b[0m")

	err := renderStatus(pane, size, status)
	if err != nil {
		return viewport{}, err
	}

	_, _ = pane.WriteString(endSync)

	return view, nil
}

func prepareTmuxImage(
	upload, pane *bytes.Buffer,
	frame *image.RGBA,
	size terminalSize,
	imageID int,
	quiet byte,
) (viewport, error) {
	width, height := frame.Rect.Dx(), frame.Rect.Dy()
	if width <= 0 || height <= 0 || width > math.MaxUint16 ||
		height > math.MaxUint16 || width > maxPixels/height {
		return viewport{}, errors.New(
			"framebuffer dimensions exceed render limit",
		)
	}

	renderSize := size

	renderSize.columns = min(size.columns, tmuxDiacriticCount)
	renderSize.rows = min(size.rows, tmuxDiacriticCount+1)

	view, err := fit(frame.Rect, renderSize)
	if err != nil {
		return viewport{}, err
	}

	padded, err := padTmuxImage(frame, view)
	if err != nil {
		return viewport{}, err
	}

	columns := padded.Rect.Dx() / size.cellWidth
	rows := padded.Rect.Dy() / size.cellHeight
	options := kitty.Options{
		Action: kitty.TransmitAndPut, Transmission: kitty.Direct,
		Format: kitty.PNG, ID: imageID, Quiet: quiet,
		VirtualPlacement: true, DoNotMoveCursor: true,
		Columns: columns, Rows: rows,
		Chunk: true, ChunkFormatter: ansi.TmuxPassthrough,
	}

	_, _ = upload.WriteString(tmuxDeleteImage(imageID))

	err = kitty.EncodeGraphics(upload, padded, &options)
	if err != nil {
		return viewport{}, fmt.Errorf("encode tmux Kitty graphics: %w", err)
	}

	writeTmuxPlaceholders(pane, imageID, columns, rows)

	return view, nil
}

func padTmuxImage(frame *image.RGBA, view viewport) (*image.RGBA, error) {
	columns := (view.width + view.cellWidth - 1) / view.cellWidth
	rows := (view.height + view.cellHeight - 1) / view.cellHeight

	if columns < 1 || rows < 1 || columns > tmuxDiacriticCount ||
		rows > tmuxDiacriticCount {
		return nil, errors.New(
			"tmux placeholder dimensions exceed diacritic limit",
		)
	}

	width, height := columns*view.cellWidth, rows*view.cellHeight
	if width > maxPixels/height {
		return nil, errors.New("padded tmux image exceeds render limit")
	}

	padded := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.ApproxBiLinear.Scale(
		padded,
		image.Rect(0, 0, view.width, view.height),
		frame,
		frame.Rect,
		draw.Src,
		nil,
	)

	return padded, nil
}

func writeTmuxPlaceholders(output *bytes.Buffer, imageID, columns, rows int) {
	_, _ = fmt.Fprintf(
		output,
		"\x1b[38;2;%d;%d;%dm",
		imageID>>tmuxImageRedShift,
		(imageID>>tmuxImageGreenShift)&math.MaxUint8,
		imageID&math.MaxUint8,
	)

	for row := range rows {
		_, _ = fmt.Fprintf(output, "\x1b[%d;1H", row+1)

		for column := range columns {
			_, _ = output.WriteRune(kitty.Placeholder)
			_, _ = output.WriteRune(kitty.Diacritic(row))
			_, _ = output.WriteRune(kitty.Diacritic(column))
		}
	}
}

func newTmuxImageID(excluded []int) (int, error) {
	for {
		var entropy [3]byte

		_, err := rand.Read(entropy[:])
		if err != nil {
			return 0, fmt.Errorf("allocate tmux graphics ID: %w", err)
		}

		id := int(entropy[0])<<16 | int(entropy[1])<<8 | int(entropy[2])
		if id <= 1 || id == 31 || slices.Contains(excluded, id) {
			continue
		}

		return id, nil
	}
}

func tmuxDeleteImage(id int) string {
	options := kitty.Options{
		Action: kitty.Delete, Delete: kitty.DeleteID,
		DeleteResources: true, ID: id, Quiet: graphicsQuiet,
	}

	return ansi.TmuxPassthrough(ansi.KittyGraphics(nil, options.Options()...))
}

func writeTmuxGraphics(writer io.Writer, sequence string) error {
	if sequence == "" {
		return nil
	}

	count, err := io.WriteString(writer, sequence)
	if count != len(sequence) && err == nil {
		err = io.ErrShortWrite
	}

	if err != nil {
		return fmt.Errorf("write tmux graphics: %w", err)
	}

	return nil
}

func recoverTmuxGraphics(writer io.Writer) error {
	outerError := writeTmuxGraphics(writer, "\x18\x1b\\\x1b\\")
	innerError := writeTmuxGraphics(
		writer, ansi.TmuxPassthrough("\x18\x1b\\"+endSync),
	)
	paneError := writeTmuxGraphics(writer, "\x18\x1b\\\x1b[0m"+endSync)

	return errors.Join(outerError, innerError, paneError)
}
