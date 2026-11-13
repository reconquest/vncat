package main

import (
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
	if width <= 0 || height <= 0 || !size.ready() {
		return viewport{}, errors.New("terminal pixel dimensions unavailable")
	}
	if size.columns > 65535/size.cellWidth ||
		size.rows > 65535/size.cellHeight {
		return viewport{}, errors.New("terminal dimensions exceed render limit")
	}
	availableWidth := size.columns * size.cellWidth
	availableHeight := size.rows * size.cellHeight
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

func render(w io.Writer, frame *image.RGBA, size terminalSize) (viewport, error) {
	view, err := fit(frame.Rect, size)
	if err != nil {
		return viewport{}, err
	}
	scaled := image.NewRGBA(image.Rect(0, 0, view.width, view.height))
	draw.ApproxBiLinear.Scale(scaled, scaled.Rect, frame, frame.Rect, draw.Src, nil)
	if _, err := io.WriteString(w, "\x1b_Ga=d,d=I,i=1,q=2\x1b\\\x1b[H"); err != nil {
		return viewport{}, err
	}
	options := kitty.Options{
		Action: kitty.TransmitAndPut, Transmission: kitty.Direct,
		Format: kitty.PNG, ID: 1, PlacementID: 1,
		Quiet: 1, Chunk: true, DoNotMoveCursor: true,
	}
	if err := kitty.EncodeGraphics(w, scaled, &options); err != nil {
		return viewport{}, fmt.Errorf("write Kitty graphics: %w", err)
	}
	return view, nil
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
