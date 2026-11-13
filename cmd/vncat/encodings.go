package main

import (
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"io"
)

func (c *rfbClient) update() (bool, error) {
	var header [3]byte
	if _, err := io.ReadFull(c.conn, header[:]); err != nil {
		return false, err
	}
	count := int(wireOrder.Uint16(header[1:]))
	resized := false
	budget := maxPixels * 4
	for i := 0; i < count; i++ {
		var raw [12]byte
		if _, err := io.ReadFull(c.conn, raw[:]); err != nil {
			return false, err
		}
		x, y := int(wireOrder.Uint16(raw[:2])), int(wireOrder.Uint16(raw[2:4]))
		width, height := int(wireOrder.Uint16(raw[4:6])), int(wireOrder.Uint16(raw[6:8]))
		encoding := int32(wireOrder.Uint32(raw[8:]))
		if encoding == -224 {
			return resized, nil
		}
		if height > 0 && width > budget/height {
			return false, errors.New("framebuffer update exceeds pixel budget")
		}
		budget -= width * height
		switch encoding {
		case -223:
			if err := c.resize(width, height); err != nil {
				return false, err
			}
			resized = true
			continue
		}
		rect := image.Rect(x, y, x+width, y+height)
		if width == 0 || height == 0 || !rect.In(c.frame.Rect) {
			return false, fmt.Errorf("rectangle %v outside framebuffer %v", rect, c.frame.Rect)
		}
		var err error
		switch encoding {
		case 0:
			err = c.raw(rect)
		case 1:
			err = c.copyRect(rect)
		case 5:
			err = c.hextile(rect)
		default:
			err = fmt.Errorf("unsupported rectangle encoding %d", encoding)
		}
		if err != nil {
			return false, err
		}
	}
	return resized, nil
}

func (c *rfbClient) raw(rect image.Rectangle) error {
	row := make([]byte, rect.Dx()*4)
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		if _, err := io.ReadFull(c.conn, row); err != nil {
			return fmt.Errorf("raw pixels: %w", err)
		}
		offset := c.frame.PixOffset(rect.Min.X, y)
		for x := 0; x < len(row); x += 4 {
			copy(c.frame.Pix[offset+x:offset+x+4], []byte{row[x+2], row[x+1], row[x], 255})
		}
	}
	return nil
}

func (c *rfbClient) copyRect(rect image.Rectangle) error {
	var raw [4]byte
	if _, err := io.ReadFull(c.conn, raw[:]); err != nil {
		return err
	}
	origin := image.Pt(int(wireOrder.Uint16(raw[:2])), int(wireOrder.Uint16(raw[2:])))
	source := image.Rectangle{Min: origin, Max: origin.Add(rect.Size())}
	if !source.In(c.frame.Rect) {
		return errors.New("CopyRect source outside framebuffer")
	}
	draw.Draw(c.frame, rect, c.frame, origin, draw.Src)
	return nil
}

func (c *rfbClient) pixel() (color.RGBA, error) {
	var raw [4]byte
	_, err := io.ReadFull(c.conn, raw[:])
	return color.RGBA{R: raw[2], G: raw[1], B: raw[0], A: 255}, err
}

func (c *rfbClient) hextile(rect image.Rectangle) error {
	var background, foreground color.RGBA
	backgroundValid, foregroundValid := false, false
	for y := rect.Min.Y; y < rect.Max.Y; y += 16 {
		for x := rect.Min.X; x < rect.Max.X; x += 16 {
			tile := image.Rect(x, y, min(x+16, rect.Max.X), min(y+16, rect.Max.Y))
			var flags [1]byte
			if _, err := io.ReadFull(c.conn, flags[:]); err != nil {
				return err
			}
			if flags[0]&1 != 0 {
				if err := c.raw(tile); err != nil {
					return err
				}
				backgroundValid, foregroundValid = false, false
				continue
			}
			if flags[0]&^byte(31) != 0 || flags[0]&20 == 20 {
				return errors.New("invalid Hextile flags")
			}
			if flags[0]&2 != 0 {
				value, err := c.pixel()
				if err != nil {
					return err
				}
				background, backgroundValid = value, true
			}
			if !backgroundValid {
				return errors.New("Hextile background unspecified")
			}
			draw.Draw(c.frame, tile, &image.Uniform{C: background}, image.Point{}, draw.Src)
			if flags[0]&4 != 0 {
				value, err := c.pixel()
				if err != nil {
					return err
				}
				foreground, foregroundValid = value, true
			}
			if flags[0]&16 != 0 {
				foregroundValid = false
			}
			if flags[0]&8 == 0 {
				continue
			}
			var count [1]byte
			if _, err := io.ReadFull(c.conn, count[:]); err != nil {
				return err
			}
			for i := 0; i < int(count[0]); i++ {
				value := foreground
				if flags[0]&16 != 0 {
					var err error
					value, err = c.pixel()
					if err != nil {
						return err
					}
				} else if !foregroundValid {
					return errors.New("Hextile foreground unspecified")
				}
				var geometry [2]byte
				if _, err := io.ReadFull(c.conn, geometry[:]); err != nil {
					return err
				}
				left, top := x+int(geometry[0]>>4), y+int(geometry[0]&15)
				sub := image.Rect(left, top, left+int(geometry[1]>>4)+1, top+int(geometry[1]&15)+1)
				if !sub.In(tile) {
					return errors.New("Hextile subrectangle outside tile")
				}
				draw.Draw(c.frame, sub, &image.Uniform{C: value}, image.Point{}, draw.Src)
			}
		}
	}
	return nil
}
