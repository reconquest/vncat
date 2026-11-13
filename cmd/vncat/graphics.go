package main

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"io"

	"github.com/charmbracelet/x/ansi/kitty"
	"golang.org/x/image/draw"
)

type terminalSize struct {
	columns, rows         int
	cellWidth, cellHeight int
}

type viewport struct {
	width, height             int
	remoteWidth, remoteHeight int
	cellWidth, cellHeight     int
}

func (s terminalSize) ready() bool {
	return s.columns > 0 && s.rows > 0 &&
		s.cellWidth > 0 && s.cellHeight > 0
}

func fit(frame image.Rectangle, size terminalSize) (viewport, error) {
	width, height := frame.Dx(), frame.Dy()
	if width <= 0 || height <= 0 || !size.ready() || size.rows < 2 {
		return viewport{}, errors.New("terminal pixel dimensions unavailable")
	}
	if size.columns > 65535/size.cellWidth ||
		size.rows > 65535/size.cellHeight {
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

func render(w io.Writer, frame *image.RGBA, size terminalSize, readonly bool) (view viewport, result error) {
	var output bytes.Buffer
	output.WriteString(beginSync + "\x1b_Ga=d,d=I,i=1,q=2\x1b\\\x1b[2J\x1b[H")
	if err := renderStatus(&output, size, readonly); err != nil {
		return viewport{}, err
	}
	if frame != nil && size.rows >= 2 && size.columns >= 1 {
		var err error
		view, err = fit(frame.Rect, size)
		if err != nil {
			return viewport{}, err
		}
		scaled := image.NewRGBA(image.Rect(0, 0, view.width, view.height))
		draw.ApproxBiLinear.Scale(scaled, scaled.Rect, frame, frame.Rect, draw.Src, nil)
		options := kitty.Options{
			Action: kitty.TransmitAndPut, Transmission: kitty.Direct,
			Format: kitty.PNG, ID: 1, PlacementID: 1,
			Quiet: 1, Chunk: true, DoNotMoveCursor: true,
		}
		if err := kitty.EncodeGraphics(&output, scaled, &options); err != nil {
			return viewport{}, fmt.Errorf("encode Kitty graphics: %w", err)
		}
	}
	defer func() {
		sequence := endSync
		if result != nil {
			sequence = "\x1b\\" + sequence
		}
		n, err := io.WriteString(w, sequence)
		if n < len(sequence) && err == nil {
			err = io.ErrShortWrite
		}
		result = errors.Join(result, err)
		if result != nil {
			view = viewport{}
		}
	}()
	_, result = output.WriteTo(w)
	return view, result
}

func renderStatus(w io.Writer, size terminalSize, readonly bool) error {
	if size.columns < 1 || size.rows < 1 {
		return nil
	}
	mode := "interactive"
	if readonly {
		mode = "readonly"
	}
	status := "Ctrl-] quit | " + mode
	status = status[:min(len(status), size.columns)]
	_, err := fmt.Fprintf(w, "\x1b[%d;1H\x1b[2K%s\x1b[H", size.rows, status)
	return err
}

func (v viewport) point(x, y int) (uint16, uint16, bool) {
	if v.width <= 0 || v.height <= 0 || v.cellWidth <= 0 || v.cellHeight <= 0 {
		return 0, 0, false
	}
	inside := x >= 0 && y >= 0 &&
		x < (v.width+v.cellWidth-1)/v.cellWidth &&
		y < (v.height+v.cellHeight-1)/v.cellHeight
	x = max(-1, min(v.width/v.cellWidth, x))
	y = max(-1, min(v.height/v.cellHeight, y))
	px := max(0, x*v.cellWidth+v.cellWidth/2)
	py := max(0, y*v.cellHeight+v.cellHeight/2)
	remoteX := int64(px) * int64(v.remoteWidth) / int64(v.width)
	remoteY := int64(py) * int64(v.remoteHeight) / int64(v.height)
	return uint16(min(int64(v.remoteWidth-1), remoteX)),
		uint16(min(int64(v.remoteHeight-1), remoteY)), inside
}
