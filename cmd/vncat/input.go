package main

import (
	"encoding/binary"
	"errors"
	"slices"
	"unicode"
	"unicode/utf8"

	uv "github.com/charmbracelet/ultraviolet"
)

type legacyKeyEvent uv.Key

type inputState struct {
	client        *rfbClient
	kitty         bool
	held          map[rune]uint32
	heldModifiers map[rune]uv.KeyMod
	modifiers     uv.KeyMod
	buttons       byte
	x, y          uint16
	view          viewport
}

type modifierKey struct {
	mod    uv.KeyMod
	symbol uint32
}

const modifierCount = 6

func modifierKeys() [modifierCount]modifierKey {
	return [modifierCount]modifierKey{
		{uv.ModShift, 0xffe1},
		{uv.ModCtrl, 0xffe3},
		{uv.ModAlt, 0xffe9},
		{uv.ModSuper, 0xffeb},
		{uv.ModHyper, 0xffed},
		{uv.ModMeta, 0xffe7},
	}
}

func (state *inputState) handle(event uv.Event) (bool, error) {
	switch event := event.(type) {
	case uv.KeyboardEnhancementsEvent:
		state.kitty = event.SupportsKeyReleases()

	case legacyKeyEvent:
		return state.press(uv.Key(event), false)

	case uv.KeyPressEvent:
		return state.press(uv.Key(event), state.kitty)

	case uv.KeyReleaseEvent:
		if !state.client.readonly {
			return false, state.keyboard(uv.Key(event), false, true)
		}

	case pixelMouseEvent:
		point, inside := state.view.pixelPoint(event.x, event.y)

		return false, state.forwardMouse(
			event.event,
			mouseTarget{point, inside},
		)

	case uv.MouseEvent:
		return false, state.mouse(event)

	default:
		return false, nil
	}

	return false, nil
}

func (state *inputState) press(key uv.Key, hold bool) (bool, error) {
	if key.Code == ']' && key.Mod.Contains(uv.ModCtrl) {
		return true, nil
	}

	if state.client.readonly {
		return false, nil
	}

	return false, state.keyboard(key, true, hold)
}

func (state *inputState) keyboard(key uv.Key, down, hold bool) error {
	return state.transitionKey(key, keyTransition{down: down, hold: hold})
}

func (state *inputState) transitionKey(
	key uv.Key,
	transition keyTransition,
) error {
	if state.held == nil {
		state.held = make(map[rune]uint32)
	}

	modifierForMod := modifierFor(key.Code)
	if modifierForMod != 0 {
		return state.modifier(
			key,
			modifierForMod,
			transition,
		)
	}

	err := state.syncModifiers(key.Mod)
	if err != nil {
		return err
	}

	if !transition.down {
		return state.releaseKey(key.Code)
	}

	if key.Code == uv.KeyExtended {
		return state.typeText(key.Text)
	}

	return state.pressSymbol(key, transition)
}

func (state *inputState) pressSymbol(
	key uv.Key,
	transition keyTransition,
) error {
	symbol := keySymbol(key)
	if symbol == 0 {
		return nil
	}

	previous, held := state.held[key.Code]
	if held {
		symbol = previous
	}

	if !held && len(state.held) >= 256 {
		return errors.New("too many held keys")
	}

	state.held[key.Code] = symbol

	err := state.client.key(symbol, true)
	if err != nil {
		return err
	}

	if transition.hold {
		return nil
	}

	err = state.client.key(symbol, false)
	if err != nil {
		return err
	}

	delete(state.held, key.Code)

	return state.syncModifiers(0)
}

func (state *inputState) syncModifiers(desired uv.KeyMod) error {
	for _, mod := range state.heldModifiers {
		desired |= mod
	}

	for _, key := range modifierKeys() {
		before, after := state.modifiers.Contains(
			key.mod,
		), desired.Contains(
			key.mod,
		)
		if before == after {
			continue
		}

		err := state.transitionModifier(key, keyTransition{down: after})
		if err != nil {
			return err
		}
	}

	return nil
}

func (state *inputState) transitionModifier(
	key modifierKey,
	transition keyTransition,
) error {
	if transition.down {
		state.modifiers |= key.mod
	}

	err := state.client.key(key.symbol, transition.down)
	if err != nil {
		return err
	}

	if !transition.down {
		state.modifiers &^= key.mod
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

	default:
		return 0
	}
}

func keySymbol(key uv.Key) uint32 {
	specialKeys := specialKeySymbols()

	if symbol, ok := specialKeys[key.Code]; ok {
		return symbol
	}

	if key.Code >= uv.KeyF1 && key.Code <= uv.KeyF35 {
		return uint32(0xffbe + key.Code - uv.KeyF1)
	}

	if key.Code >= uv.KeyKp0 && key.Code <= uv.KeyKp9 {
		return uint32(0xffb0 + key.Code - uv.KeyKp0)
	}

	return runeSymbol(textCode(key))
}

func textCode(key uv.Key) rune {
	code := key.Code
	switch {
	case key.ShiftedCode != 0 && key.Mod.Contains(uv.ModShift):
		code = key.ShiftedCode

	case utf8.RuneCountInString(key.Text) == 1:
		code, _ = utf8.DecodeRuneInString(key.Text)

	case key.Mod.Contains(uv.ModShift):
		code = unicode.ToUpper(code)

	default:
	}

	return code
}

func runeSymbol(code rune) uint32 {
	if !utf8.ValidRune(code) || code < 32 || code == 127 {
		return 0
	}

	if code <= latin1Maximum {
		return uint32(code)
	}

	return unicodeKeysymMask | uint32(code)
}

type mouseTarget struct {
	point  remotePoint
	inside bool
}

func (state *inputState) mouse(event uv.MouseEvent) error {
	mouse := event.Mouse()
	point, inside := state.view.point(mouse.X, mouse.Y)

	return state.forwardMouse(event, mouseTarget{point, inside})
}

func (state *inputState) forwardMouse(
	event uv.MouseEvent,
	target mouseTarget,
) error {
	if state.client.readonly {
		return nil
	}

	point := target.point
	mouse := event.Mouse()
	_, release := event.(uv.MouseReleaseEvent)

	if !target.inside && state.buttons == 0 {
		return nil
	}

	if state.view.width == 0 {
		if !release {
			return nil
		}

		point = remotePoint{x: state.x, y: state.y}
	}

	state.x, state.y = point.x, point.y

	const mouseModifiers = uv.ModShift | uv.ModAlt | uv.ModCtrl

	desired := state.modifiers&^mouseModifiers | mouse.Mod&mouseModifiers

	err := state.syncModifiers(desired)
	if err != nil {
		return err
	}

	return state.mouseButton(event, point)
}

func (state *inputState) mouseButton(
	event uv.MouseEvent,
	point remotePoint,
) error {
	mouse := event.Mouse()
	button := byte(0)

	if mouse.Button >= uv.MouseLeft && mouse.Button <= uv.MouseWheelRight {
		button = 1 << uint(mouse.Button-uv.MouseLeft)
	}

	switch event.(type) {
	case uv.MouseClickEvent:
		state.buttons |= button & pointerButtonMask

	case uv.MouseReleaseEvent:
		if mouse.Button == uv.MouseNone {
			state.buttons = 0
		} else {
			state.buttons &^= button
		}

	case uv.MouseWheelEvent:
		err := state.client.pointer(state.buttons|button, point.x, point.y)
		if err != nil {
			return err
		}

	default:
	}

	return state.client.pointer(state.buttons, point.x, point.y)
}

func (state *inputState) release() error {
	if state.client.readonly {
		return nil
	}

	symbols := state.releaseSymbols()
	msg := state.releaseMessage(symbols)
	clear(state.held)
	clear(state.heldModifiers)

	state.modifiers, state.buttons = 0, 0

	if len(msg) == 0 {
		return nil
	}

	return state.client.write(msg)
}

func (state *inputState) releaseSymbols() []uint32 {
	symbols := make([]uint32, 0, len(state.held)+modifierCount)
	for _, symbol := range state.held {
		symbols = append(symbols, symbol)
	}

	slices.Sort(symbols)

	for _, key := range modifierKeys() {
		if state.modifiers.Contains(key.mod) {
			symbols = append(symbols, key.symbol)
		}
	}

	return symbols
}

func (state *inputState) releaseMessage(symbols []uint32) []byte {
	msg := make([]byte, 0, len(symbols)*8+pointerMessageSize)
	for _, symbol := range symbols {
		msg = append(msg, clientKeyMessage, 0, 0, 0)
		msg = binary.BigEndian.AppendUint32(msg, symbol)
	}

	if state.buttons != 0 {
		msg = append(msg, clientPointerMessage, 0)
		msg = binary.BigEndian.AppendUint16(msg, state.x)
		msg = binary.BigEndian.AppendUint16(msg, state.y)
	}

	return msg
}

func (state *inputState) modifier(
	key uv.Key,
	mod uv.KeyMod,
	transition keyTransition,
) error {
	if transition.hold {
		if state.heldModifiers == nil {
			state.heldModifiers = make(map[rune]uv.KeyMod)
		}

		if transition.down {
			state.heldModifiers[key.Code] = mod
		} else {
			delete(state.heldModifiers, key.Code)
		}
	}

	desired := key.Mod
	if transition.down {
		desired |= mod
	} else {
		desired &^= mod
	}

	return state.syncModifiers(desired)
}

func (state *inputState) releaseKey(code rune) error {
	symbol, ok := state.held[code]
	if !ok {
		return nil
	}

	err := state.client.key(symbol, false)
	if err != nil {
		return err
	}

	delete(state.held, code)

	return nil
}

func (state *inputState) typeText(text string) error {
	for _, code := range text {
		symbol := runeSymbol(code)
		if symbol == 0 {
			continue
		}

		err := state.client.key(symbol, true)
		if err != nil {
			return err
		}

		err = state.client.key(symbol, false)
		if err != nil {
			return err
		}
	}

	return state.syncModifiers(0)
}

func specialKeySymbols() map[rune]uint32 {
	return map[rune]uint32{
		uv.KeyBackspace:      keysymBackspace,
		uv.KeyTab:            keysymTab,
		uv.KeyEnter:          keysymEnter,
		uv.KeyEscape:         keysymEscape,
		uv.KeyHome:           keysymHome,
		uv.KeyLeft:           keysymLeft,
		uv.KeyUp:             keysymUp,
		uv.KeyRight:          keysymRight,
		uv.KeyDown:           keysymDown,
		uv.KeyPgUp:           keysymPgUp,
		uv.KeyPgDown:         keysymPgDown,
		uv.KeyEnd:            keysymEnd,
		uv.KeyBegin:          keysymBegin,
		uv.KeyInsert:         keysymInsert,
		uv.KeyDelete:         keysymDelete,
		uv.KeyKpEnter:        keysymKpEnter,
		uv.KeyCapsLock:       keysymCapsLock,
		uv.KeyNumLock:        keysymNumLock,
		uv.KeyScrollLock:     keysymScrollLock,
		uv.KeyPrintScreen:    keysymPrintScreen,
		uv.KeyPause:          keysymPause,
		uv.KeyMenu:           keysymMenu,
		uv.KeyKpEqual:        keysymKpEqual,
		uv.KeyKpMultiply:     keysymKpMultiply,
		uv.KeyKpPlus:         keysymKpPlus,
		uv.KeyKpMinus:        keysymKpMinus,
		uv.KeyKpSep:          keysymKpSep,
		uv.KeyKpComma:        keysymKpComma,
		uv.KeyKpDecimal:      keysymKpDecimal,
		uv.KeyKpDivide:       keysymKpDivide,
		uv.KeyKpHome:         keysymKpHome,
		uv.KeyKpEnd:          keysymKpEnd,
		uv.KeyKpBegin:        keysymKpBegin,
		uv.KeyKpLeft:         keysymKpLeft,
		uv.KeyKpUp:           keysymKpUp,
		uv.KeyKpRight:        keysymKpRight,
		uv.KeyKpDown:         keysymKpDown,
		uv.KeyKpPgUp:         keysymKpPgUp,
		uv.KeyKpPgDown:       keysymKpPgDown,
		uv.KeyKpInsert:       keysymKpInsert,
		uv.KeyKpDelete:       keysymKpDelete,
		uv.KeyIsoLevel3Shift: keysymIsoLevel3Shift,
		uv.KeyIsoLevel5Shift: keysymIsoLevel5Shift,
	}
}
