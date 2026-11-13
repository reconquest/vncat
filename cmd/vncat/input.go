package main

import (
	"errors"
	"sort"
	"unicode"
	"unicode/utf8"

	uv "github.com/charmbracelet/ultraviolet"
)

type legacyKeyEvent uv.Key

type inputState struct {
	client    *rfbClient
	kitty     bool
	held      map[rune]uint32
	modifiers uv.KeyMod
	buttons   byte
	x, y      uint16
	view      viewport
}

var modifierKeys = []struct {
	mod    uv.KeyMod
	symbol uint32
}{
	{uv.ModShift, 0xffe1}, {uv.ModCtrl, 0xffe3}, {uv.ModAlt, 0xffe9},
	{uv.ModSuper, 0xffeb}, {uv.ModHyper, 0xffed}, {uv.ModMeta, 0xffe7},
}

var specialKeys = map[rune]uint32{
	uv.KeyBackspace: 0xff08, uv.KeyTab: 0xff09, uv.KeyEnter: 0xff0d,
	uv.KeyEscape: 0xff1b, uv.KeyHome: 0xff50, uv.KeyLeft: 0xff51,
	uv.KeyUp: 0xff52, uv.KeyRight: 0xff53, uv.KeyDown: 0xff54,
	uv.KeyPgUp: 0xff55, uv.KeyPgDown: 0xff56, uv.KeyEnd: 0xff57,
	uv.KeyBegin: 0xff58, uv.KeyInsert: 0xff63, uv.KeyDelete: 0xffff,
	uv.KeyKpEnter: 0xff8d, uv.KeyCapsLock: 0xffe5, uv.KeyNumLock: 0xff7f,
	uv.KeyScrollLock: 0xff14, uv.KeyPrintScreen: 0xff61, uv.KeyPause: 0xff13,
	uv.KeyMenu:    0xff67,
	uv.KeyKpEqual: 0xffbd, uv.KeyKpMultiply: 0xffaa,
	uv.KeyKpPlus: 0xffab, uv.KeyKpMinus: 0xffad,
	uv.KeyKpDecimal: 0xffae, uv.KeyKpDivide: 0xffaf,
	uv.KeyKpHome: 0xff95, uv.KeyKpEnd: 0xff9c,
	uv.KeyKpLeft: 0xff96, uv.KeyKpUp: 0xff97,
	uv.KeyKpRight: 0xff98, uv.KeyKpDown: 0xff99,
	uv.KeyKpPgUp: 0xff9a, uv.KeyKpPgDown: 0xff9b,
	uv.KeyKpInsert: 0xff9e, uv.KeyKpDelete: 0xff9f,
	uv.KeyIsoLevel3Shift: 0xfe03, uv.KeyIsoLevel5Shift: 0xfe11,
}

func (s *inputState) handle(event uv.Event) (bool, error) {
	switch event := event.(type) {
	case uv.KeyboardEnhancementsEvent:
		s.kitty = event.SupportsKeyReleases()
	case legacyKeyEvent:
		return s.press(uv.Key(event), false)
	case uv.KeyPressEvent:
		return s.press(uv.Key(event), s.kitty)
	case uv.KeyReleaseEvent:
		if !s.client.readonly {
			return false, s.keyboard(uv.Key(event), false, true)
		}
	case uv.MouseEvent:
		if !s.client.readonly {
			return false, s.mouse(event)
		}
	}
	return false, nil
}

func (s *inputState) press(key uv.Key, hold bool) (bool, error) {
	if key.Code == ']' && key.Mod.Contains(uv.ModCtrl) {
		return true, nil
	}
	if s.client.readonly {
		return false, nil
	}
	return false, s.keyboard(key, true, hold)
}

func (s *inputState) keyboard(key uv.Key, down, hold bool) error {
	if s.held == nil {
		s.held = make(map[rune]uint32)
	}
	if mod := modifierFor(key.Code); mod != 0 {
		desired := key.Mod
		if down {
			desired |= mod
		} else {
			desired &^= mod
		}
		return s.syncModifiers(desired)
	}
	if err := s.syncModifiers(key.Mod); err != nil {
		return err
	}
	if !down {
		symbol, ok := s.held[key.Code]
		if !ok {
			return nil
		}
		if err := s.client.key(symbol, false); err != nil {
			return err
		}
		delete(s.held, key.Code)
		return nil
	}
	if key.Code == uv.KeyExtended {
		for _, r := range key.Text {
			symbol := runeSymbol(r)
			if symbol == 0 {
				continue
			}
			if err := s.client.key(symbol, true); err != nil {
				return err
			}
			if err := s.client.key(symbol, false); err != nil {
				return err
			}
		}
		return s.syncModifiers(0)
	}
	symbol := keySymbol(key)
	if symbol == 0 {
		return nil
	}
	previous, held := s.held[key.Code]
	if held {
		symbol = previous
	}
	if !held && len(s.held) >= 256 {
		return errors.New("too many held keys")
	}
	s.held[key.Code] = symbol
	if err := s.client.key(symbol, true); err != nil {
		return err
	}
	if hold {
		return nil
	}
	if err := s.client.key(symbol, false); err != nil {
		return err
	}
	delete(s.held, key.Code)
	return s.syncModifiers(0)
}

func (s *inputState) syncModifiers(desired uv.KeyMod) error {
	for _, key := range modifierKeys {
		before, after := s.modifiers.Contains(key.mod), desired.Contains(key.mod)
		if before == after {
			continue
		}
		if after {
			s.modifiers |= key.mod
		}
		if err := s.client.key(key.symbol, after); err != nil {
			return err
		}
		if !after {
			s.modifiers &^= key.mod
		}
	}
	return nil
}

func modifierFor(code rune) uv.KeyMod {
	switch code {
	case uv.KeyLeftShift, uv.KeyRightShift:
		return uv.ModShift
	case uv.KeyLeftCtrl, uv.KeyRightCtrl:
		return uv.ModCtrl
	case uv.KeyLeftAlt, uv.KeyRightAlt:
		return uv.ModAlt
	case uv.KeyLeftSuper, uv.KeyRightSuper:
		return uv.ModSuper
	case uv.KeyLeftHyper, uv.KeyRightHyper:
		return uv.ModHyper
	case uv.KeyLeftMeta, uv.KeyRightMeta:
		return uv.ModMeta
	}
	return 0
}

func keySymbol(key uv.Key) uint32 {
	if symbol, ok := specialKeys[key.Code]; ok {
		return symbol
	}
	if key.Code >= uv.KeyF1 && key.Code <= uv.KeyF35 {
		return uint32(0xffbe + key.Code - uv.KeyF1)
	}
	if key.Code >= uv.KeyKp0 && key.Code <= uv.KeyKp9 {
		return uint32(0xffb0 + key.Code - uv.KeyKp0)
	}
	code := key.Code
	if key.ShiftedCode != 0 && key.Mod.Contains(uv.ModShift) {
		code = key.ShiftedCode
	} else if utf8.RuneCountInString(key.Text) == 1 {
		code, _ = utf8.DecodeRuneInString(key.Text)
	} else if key.Mod.Contains(uv.ModShift) {
		code = unicode.ToUpper(code)
	}
	return runeSymbol(code)
}

func runeSymbol(code rune) uint32 {
	if !utf8.ValidRune(code) || code < 32 || code == 127 {
		return 0
	}
	if code <= 255 {
		return uint32(code)
	}
	return 0x01000000 | uint32(code)
}

func (s *inputState) mouse(event uv.MouseEvent) error {
	mouse := event.Mouse()
	x, y, inside := s.view.point(mouse.X, mouse.Y)
	_, release := event.(uv.MouseReleaseEvent)
	if !inside && s.buttons == 0 && !release {
		return nil
	}
	if s.view.width == 0 {
		return nil
	}
	s.x, s.y = x, y
	if err := s.syncModifiers(mouse.Mod); err != nil {
		return err
	}
	button := byte(0)
	if mouse.Button >= uv.MouseLeft && mouse.Button <= uv.MouseWheelRight {
		button = 1 << uint(mouse.Button-uv.MouseLeft)
	}
	switch event.(type) {
	case uv.MouseClickEvent:
		s.buttons |= button & 7
	case uv.MouseReleaseEvent:
		if button == 0 {
			s.buttons = 0
		} else {
			s.buttons &^= button
		}
	case uv.MouseWheelEvent:
		if err := s.client.pointer(s.buttons|button, x, y); err != nil {
			return err
		}
	}
	return s.client.pointer(s.buttons, x, y)
}

func (s *inputState) release() error {
	if s.client.readonly {
		return nil
	}
	symbols := make([]uint32, 0, len(s.held)+len(modifierKeys))
	for _, symbol := range s.held {
		symbols = append(symbols, symbol)
	}
	sort.Slice(symbols, func(i, j int) bool { return symbols[i] < symbols[j] })
	for _, key := range modifierKeys {
		if s.modifiers.Contains(key.mod) {
			symbols = append(symbols, key.symbol)
		}
	}
	msg := make([]byte, 0, len(symbols)*8+6)
	for _, symbol := range symbols {
		msg = append(msg, 4, 0, 0, 0, byte(symbol>>24), byte(symbol>>16), byte(symbol>>8), byte(symbol))
	}
	if s.buttons != 0 {
		msg = append(msg, 5, 0, byte(s.x>>8), byte(s.x), byte(s.y>>8), byte(s.y))
	}
	clear(s.held)
	s.modifiers, s.buttons = 0, 0
	if len(msg) == 0 {
		return nil
	}
	return s.client.write(msg)
}
