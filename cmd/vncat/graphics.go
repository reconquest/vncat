package main

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"io"
	"math"

	"github.com/charmbracelet/x/ansi/kitty"
	"golang.org/x/image/draw"
)

const graphicsQuiet = 2

type terminalSize struct {
	columns, rows         int
	cellWidth, cellHeight int
}

type viewport struct {
	width, height             int
	remoteWidth, remoteHeight int
	cellWidth, cellHeight     int
}

func (size terminalSize) ready() bool {
	return size.columns > 0 && size.rows > 0 &&
		size.cellWidth > 0 && size.cellHeight > 0
}

func fit(frame image.Rectangle, size terminalSize) (viewport, error) {
	width, height := frame.Dx(), frame.Dy()
	if width <= 0 || height <= 0 || !size.ready() || size.rows < 2 {
		return viewport{}, errors.New("terminal pixel dimensions unavailable")
	}

	if size.columns > math.MaxUint16/size.cellWidth ||
		size.rows > math.MaxUint16/size.cellHeight {
		return viewport{}, errors.New("terminal dimensions exceed render limit")
	}

	availableWidth := size.columns * size.cellWidth
	availableHeight := (size.rows - 1) * size.cellHeight

	if availableWidth > maxPixels/availableHeight {
		return viewport{}, errors.New("terminal dimensions exceed render limit")
	}

	scaledWidth := availableWidth
	scaledHeight := int(int64(availableWidth) * int64(height) / int64(width))

	if scaledHeight > availableHeight {
		scaledHeight = availableHeight
		scaledWidth = int(int64(availableHeight) * int64(width) / int64(height))
	}

	return viewport{
		width: max(1, scaledWidth), height: max(1, scaledHeight),
		remoteWidth: width, remoteHeight: height,
		cellWidth: size.cellWidth, cellHeight: size.cellHeight,
	}, nil
}

func render(
	writer io.Writer,
	frame *image.RGBA,
	size terminalSize,
	readonly bool,
	fps float64,
) (viewport, error) {
	var (
		view   viewport
		output bytes.Buffer
	)

	_, _ = output.WriteString(
		beginSync + "\x1b_Ga=d,d=I,i=1,q=2\x1b\\\x1b[2J\x1b[H",
	)

	err := renderStatus(
		&output,
		size,
		footerStatus{readonly: readonly, fps: fps},
	)
	if err != nil {
		return viewport{}, err
	}

	if frame != nil && size.rows >= 2 && size.columns >= 1 {
		view, err = prepareImage(&output, frame, size)
		if err != nil {
			return viewport{}, err
		}
	}

	err = writePresentation(writer, &output)
	if err != nil {
		return viewport{}, err
	}

	return view, nil
}

func prepareImage(
	output io.Writer,
	frame *image.RGBA,
	size terminalSize,
) (viewport, error) {
	view, err := fit(frame.Rect, size)
	if err != nil {
		return viewport{}, err
	}

	err = encodeFrame(output, frame, view, &kitty.Options{
		ID: 1, PlacementID: 1, Quiet: 1,
	})
	if err != nil {
		return viewport{}, err
	}

	return view, nil
}

func encodeFrame(
	output io.Writer,
	frame *image.RGBA,
	view viewport,
	options *kitty.Options,
) error {
	scaled := image.NewRGBA(image.Rect(0, 0, view.width, view.height))
	draw.ApproxBiLinear.Scale(
		scaled,
		scaled.Rect,
		frame,
		frame.Rect,
		draw.Src,
		nil,
	)

	options.Action = kitty.TransmitAndPut
	options.Transmission = kitty.Direct
	options.Format = kitty.PNG
	options.Chunk = true
	options.DoNotMoveCursor = true

	err := kitty.EncodeGraphics(output, scaled, options)
	if err != nil {
		return fmt.Errorf("encode Kitty graphics: %w", err)
	}

	return nil
}

func refreshStatus(
	writer io.Writer,
	size terminalSize,
	readonly bool,
	fps float64,
) error {
	var output bytes.Buffer

	_, _ = output.WriteString(beginSync)

	err := renderStatus(
		&output,
		size,
		footerStatus{readonly: readonly, fps: fps},
	)
	if err != nil {
		return err
	}

	return writePresentation(writer, &output)
}

func writePresentation(writer io.Writer, output *bytes.Buffer) error {
	_, writeError := output.WriteTo(writer)
	sequence := endSync

	if writeError != nil {
		sequence = "\x1b\\" + sequence
		writeError = fmt.Errorf("write presentation: %w", writeError)
	}

	count, syncError := io.WriteString(writer, sequence)
	if count < len(sequence) && syncError == nil {
		syncError = io.ErrShortWrite
	}

	if syncError != nil {
		syncError = fmt.Errorf("end synchronization: %w", syncError)
	}

	return errors.Join(writeError, syncError)
}

type footerStatus struct {
	readonly bool
	fps      float64
}

func renderStatus(
	writer io.Writer,
	size terminalSize,
	status footerStatus,
) error {
	if size.columns < 1 || size.rows < 1 {
		return nil
	}

	mode := "interactive"
	if status.readonly {
		mode = "readonly"
	}

	text := fmt.Sprintf("Ctrl-] quit | %s | %.1f FPS", mode, status.fps)

	text = text[:min(len(text), size.columns)]

	_, err := fmt.Fprintf(
		writer,
		"\x1b[%d;1H\x1b[2K%s\x1b[H",
		size.rows,
		text,
	)
	if err != nil {
		return fmt.Errorf("write status: %w", err)
	}

	return nil
}

type remotePoint struct{ x, y uint16 }

func (view viewport) point(column, row int) (remotePoint, bool) {
	if view.width <= 0 || view.height <= 0 || view.cellWidth <= 0 ||
		view.cellHeight <= 0 {
		return remotePoint{}, false
	}

	inside := column >= 0 && row >= 0 &&
		column < (view.width+view.cellWidth-1)/view.cellWidth &&
		row < (view.height+view.cellHeight-1)/view.cellHeight

	column = max(-1, min(view.width/view.cellWidth, column))
	row = max(-1, min(view.height/view.cellHeight, row))

	px := max(0, column*view.cellWidth+view.cellWidth/2)
	py := max(0, row*view.cellHeight+view.cellHeight/2)
	point, _ := view.pixelPoint(px, py)

	return point, inside
}

func (view viewport) pixelPoint(pixelX, pixelY int) (remotePoint, bool) {
	if view.width <= 0 || view.height <= 0 {
		return remotePoint{}, false
	}

	inside := pixelX >= 0 && pixelY >= 0 &&
		pixelX < view.width && pixelY < view.height

	clampedX := max(0, min(view.width, pixelX))
	clampedY := max(0, min(view.height, pixelY))

	remoteX := int64(clampedX) * int64(view.remoteWidth) / int64(view.width)
	remoteY := int64(clampedY) * int64(view.remoteHeight) / int64(view.height)

	return remotePoint{
		x: pointerCoordinate(min(int64(view.remoteWidth-1), remoteX)),
		y: pointerCoordinate(min(int64(view.remoteHeight-1), remoteY)),
	}, inside
}

func pointerCoordinate(value int64) uint16 {
	if value < 0 {
		return 0
	}

	if value > math.MaxUint16 {
		return math.MaxUint16
	}

	return uint16(value)
}
