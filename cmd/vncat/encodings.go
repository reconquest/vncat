package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"io"

	"golang.org/x/image/draw"
)

func (client *rfbClient) update() (bool, error) {
	var header [3]byte

	_, readError := io.ReadFull(client.conn, header[:])
	if readError != nil {
		return false, fmt.Errorf("update header: %w", readError)
	}

	count := int(binary.BigEndian.Uint16(header[1:]))
	resized := false
	budget := maxPixels * updatePixelMultiplier

	for range count {
		update, err := client.updateRectangle(&budget)
		if err != nil {
			return false, err
		}

		resized = resized || update.resized
		if update.final {
			break
		}
	}

	return resized, nil
}

type rectangleUpdate struct{ resized, final bool }

func (client *rfbClient) updateRectangle(budget *int) (rectangleUpdate, error) {
	var raw [12]byte

	_, err := io.ReadFull(client.conn, raw[:])
	if err != nil {
		return rectangleUpdate{}, fmt.Errorf("rectangle data: %w", err)
	}

	left := int(binary.BigEndian.Uint16(raw[:2]))
	top := int(binary.BigEndian.Uint16(raw[2:4]))
	width := int(binary.BigEndian.Uint16(raw[4:6]))
	height := int(binary.BigEndian.Uint16(raw[6:8]))
	encoding := binary.BigEndian.Uint32(raw[8:])

	if encoding == encodingLastRect {
		return rectangleUpdate{final: true}, nil
	}

	if height > 0 && width > *budget/height {
		return rectangleUpdate{}, errors.New(
			"framebuffer update exceeds pixel budget",
		)
	}

	*budget -= width * height
	if encoding == encodingDesktopSize {
		return rectangleUpdate{resized: true}, client.resize(width, height)
	}

	rect := image.Rect(left, top, left+width, top+height)

	err = client.decodeRectangle(rect, encoding)
	if err != nil {
		return rectangleUpdate{}, err
	}

	client.hasPixels = client.hasPixels || encoding != encodingCopyRect

	return rectangleUpdate{}, nil
}

func (client *rfbClient) decodeRectangle(
	rect image.Rectangle,
	encoding uint32,
) error {
	if rect.Empty() || !rect.In(client.frame.Rect) {
		return fmt.Errorf(
			"rectangle %v outside framebuffer %v",
			rect,
			client.frame.Rect,
		)
	}

	switch encoding {
	case encodingRaw:
		return client.raw(rect)

	case encodingCopyRect:
		return client.copyRect(rect)

	case encodingHextile:
		return client.hextile(rect)

	default:
		return fmt.Errorf("unsupported rectangle encoding %d", encoding)
	}
}

func (client *rfbClient) raw(rect image.Rectangle) error {
	row := make([]byte, 0, rect.Dx()*pixelBytes)

	row = row[:cap(row)]

	for top := rect.Min.Y; top < rect.Max.Y; top++ {
		_, err := io.ReadFull(client.conn, row)
		if err != nil {
			return fmt.Errorf("raw pixels: %w", err)
		}

		offset := client.frame.PixOffset(rect.Min.X, top)
		for x := 0; x < len(row); x += 4 {
			copy(
				client.frame.Pix[offset+x:offset+x+4],
				[]byte{row[x+2], row[x+1], row[x], 255},
			)
		}
	}

	return nil
}

func (client *rfbClient) copyRect(rect image.Rectangle) error {
	var raw [4]byte

	_, err := io.ReadFull(client.conn, raw[:])
	if err != nil {
		return fmt.Errorf("rectangle data: %w", err)
	}

	origin := image.Pt(
		int(binary.BigEndian.Uint16(raw[:2])),
		int(binary.BigEndian.Uint16(raw[2:])),
	)
	source := image.Rectangle{Min: origin, Max: origin.Add(rect.Size())}

	if !source.In(client.frame.Rect) {
		return errors.New("copyrect source outside framebuffer")
	}

	draw.Copy(client.frame, rect.Min, client.frame, source, draw.Src, nil)

	return nil
}

func (client *rfbClient) pixel() (color.RGBA, error) {
	var raw [4]byte

	_, err := io.ReadFull(client.conn, raw[:])
	if err != nil {
		return color.RGBA{}, fmt.Errorf("read pixel: %w", err)
	}

	return color.RGBA{R: raw[2], G: raw[1], B: raw[0], A: opaqueAlpha}, nil
}

type hextileState struct {
	background, foreground           color.RGBA
	backgroundValid, foregroundValid bool
}

func (client *rfbClient) hextile(rect image.Rectangle) error {
	const tileSize = 16

	var state hextileState

	for top := rect.Min.Y; top < rect.Max.Y; top += tileSize {
		for left := rect.Min.X; left < rect.Max.X; left += tileSize {
			tile := image.Rect(left, top,
				min(left+tileSize, rect.Max.X),
				min(top+tileSize, rect.Max.Y))

			err := state.tile(client, tile)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (state *hextileState) tile(client *rfbClient, tile image.Rectangle) error {
	var flags [1]byte

	_, readError := io.ReadFull(client.conn, flags[:])
	if readError != nil {
		return fmt.Errorf("hextile flags: %w", readError)
	}

	if flags[0]&1 != 0 {
		return state.rawTile(client, tile)
	}

	if flags[0]&^byte(hextileMask) != 0 || flags[0]&20 == 20 {
		return errors.New("invalid Hextile flags")
	}

	colorError := state.colors(client, tile, flags[0])
	if colorError != nil {
		return colorError
	}

	if flags[0]&8 == 0 {
		return nil
	}

	return state.subrectangles(client, tile, flags[0])
}

func (state *hextileState) rawTile(
	client *rfbClient,
	tile image.Rectangle,
) error {
	err := client.raw(tile)
	if err != nil {
		return err
	}

	state.backgroundValid, state.foregroundValid = false, false

	return nil
}

func (state *hextileState) subrectangles(
	client *rfbClient, tile image.Rectangle, flags byte,
) error {
	var count [1]byte

	_, readError := io.ReadFull(client.conn, count[:])
	if readError != nil {
		return fmt.Errorf("hextile count: %w", readError)
	}

	for range count[0] {
		value, colorError := state.subrectangleColor(client, flags)
		if colorError != nil {
			return colorError
		}

		err := client.hextileSubrectangle(tile, value)
		if err != nil {
			return err
		}
	}

	return nil
}

func (client *rfbClient) hextileSubrectangle(
	tile image.Rectangle,
	value color.RGBA,
) error {
	var geometry [2]byte

	_, err := io.ReadFull(client.conn, geometry[:])
	if err != nil {
		return fmt.Errorf("hextile geometry: %w", err)
	}

	left := tile.Min.X + int(geometry[0]>>nibbleBits)
	top := tile.Min.Y + int(geometry[0]&nibbleMask)
	sub := image.Rect(
		left,
		top,
		left+int(geometry[1]>>nibbleBits)+1,
		top+int(geometry[1]&nibbleMask)+1,
	)

	if !sub.In(tile) {
		return errors.New("hextile subrectangle outside tile")
	}

	draw.Draw(
		client.frame,
		sub,
		&image.Uniform{C: value},
		image.Point{},
		draw.Src,
	)

	return nil
}

func (state *hextileState) colors(
	client *rfbClient,
	tile image.Rectangle,
	flags byte,
) error {
	if flags&2 != 0 {
		value, err := client.pixel()
		if err != nil {
			return err
		}

		state.background, state.backgroundValid = value, true
	}

	if !state.backgroundValid {
		return errors.New("hextile background unspecified")
	}

	draw.Draw(
		client.frame,
		tile,
		&image.Uniform{C: state.background},
		image.Point{},
		draw.Src,
	)

	return state.foregroundColor(client, flags)
}

func (state *hextileState) foregroundColor(
	client *rfbClient,
	flags byte,
) error {
	if flags&4 != 0 {
		value, err := client.pixel()
		if err != nil {
			return err
		}

		state.foreground, state.foregroundValid = value, true
	}

	if flags&16 != 0 {
		state.foregroundValid = false
	}

	return nil
}

func (state *hextileState) subrectangleColor(
	client *rfbClient,
	flags byte,
) (color.RGBA, error) {
	if flags&16 != 0 {
		return client.pixel()
	}

	if !state.foregroundValid {
		return color.RGBA{}, errors.New("hextile foreground unspecified")
	}

	return state.foreground, nil
}
