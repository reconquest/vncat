package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	uv "github.com/charmbracelet/ultraviolet"
)

type decodedEvent struct{ value uv.Event }

type eventDecoder struct {
	decoder uv.EventDecoder
	pending []byte
	kitty   bool
}

func (decoder *eventDecoder) feed(
	data []byte,
	expired bool,
) ([]uv.Event, error) {
	decoder.pending = append(decoder.pending, data...)

	events := make([]uv.Event, 0)

	for len(decoder.pending) > 0 {
		size, err := decoder.nextSequence(inputTimeout{expired: expired})
		if err != nil {
			return nil, err
		}

		if size == 0 {
			break
		}

		events, err = decoder.decodeSequence(decoder.pending[:size], events)
		if err != nil {
			return nil, err
		}
	}

	return events, nil
}

type inputTimeout struct{ expired bool }

func (decoder *eventDecoder) nextSequence(timeout inputTimeout) (int, error) {
	size := sequenceLength(decoder.pending)
	if size > sequenceLimit ||
		(size == 0 && len(decoder.pending) > sequenceLimit) {
		return 0, errors.New("terminal input sequence exceeds limit")
	}

	if size != 0 || !timeout.expired {
		return size, nil
	}

	if len(decoder.pending) > 2 || decoder.pending[0] != 27 {
		decoder.pending = nil

		return 0, nil
	}

	return len(decoder.pending), nil
}

func (decoder *eventDecoder) correctAlternateKeys(
	sequence []byte,
	event *decodedEvent,
) {
	key, ok := event.value.(uv.KeyEvent)
	if !ok || !bytes.HasPrefix(sequence, []byte("\x1b[")) ||
		sequence[len(sequence)-1] != 'u' {
		return
	}

	end := bytes.IndexByte(sequence, ';')
	if end < 0 {
		end = len(sequence) - 1
	}

	codes := sequence[:end]
	if bytes.Count(codes, []byte{':'}) != alternateSeparators {
		return
	}

	base := bytes.LastIndexByte(codes, ':')
	withoutBase := append(bytes.Clone(sequence[:base]), sequence[end:]...)
	_, corrected := decoder.decoder.Decode(withoutBase)

	switch corrected := corrected.(type) {
	case uv.KeyPressEvent:
		corrected.BaseCode = key.Key().BaseCode

		event.value = corrected

		return

	case uv.KeyReleaseEvent:
		corrected.BaseCode = key.Key().BaseCode

		event.value = corrected

		return
	}
}

func (decoder *eventDecoder) correctKittyModifiers(
	sequence []byte,
	event *decodedEvent,
) {
	if enhancements, ok := event.value.(uv.KeyboardEnhancementsEvent); ok {
		decoder.kitty = enhancements.Flags != 0

		return
	}

	decoder.correctNavigationModifiers(sequence, event)
}

func (decoder *eventDecoder) correctNavigationModifiers(
	sequence []byte,
	event *decodedEvent,
) {
	key, ok := event.value.(uv.KeyEvent)
	if !ok || !bytes.HasPrefix(sequence, []byte("\x1b[")) {
		return
	}

	switch sequence[len(sequence)-1] {
	case 'A', 'B', 'C', 'D', 'E', 'F', 'H', 'P', 'Q', 'S', 'Z', '~':
	default:
		return
	}

	code, modifiers, ok := bytes.Cut(sequence[2:len(sequence)-1], []byte{';'})
	if !ok || bytes.Equal(code, []byte("27")) {
		return
	}

	modifiers, _, _ = bytes.Cut(modifiers, []byte{';'})
	if !decoder.kitty && !bytes.ContainsRune(modifiers, ':') {
		return
	}

	correctedModifierEvent(key, event)
}

func correctedModifierEvent(key uv.KeyEvent, event *decodedEvent) {
	corrected := key.Key()

	corrected.Mod &^= uv.ModSuper | uv.ModMeta

	if key.Key().Mod.Contains(uv.ModMeta) {
		corrected.Mod |= uv.ModSuper
	}

	if key.Key().Mod.Contains(uv.ModSuper) {
		corrected.Mod |= uv.ModMeta
	}

	if _, release := event.value.(uv.KeyReleaseEvent); release {
		event.value = uv.KeyReleaseEvent(corrected)

		return
	}

	event.value = uv.KeyPressEvent(corrected)
}

func runeLength(data []byte) int {
	if !utf8.FullRune(data) {
		return 0
	}

	_, size := utf8.DecodeRune(data)

	return size
}

func sequenceLength(data []byte) int {
	prefix := 0
	for len(data)-prefix >= 2 && data[prefix] == controlEscape &&
		data[prefix+1] == controlEscape {
		prefix++
		if prefix >= sequenceLimit {
			return sequenceLimit + 1
		}
	}

	size := unprefixedSequenceLength(data[prefix:])
	if size == 0 {
		return 0
	}

	return prefix + size
}

func unprefixedSequenceLength(data []byte) int {
	if len(data) == 0 {
		return 0
	}

	if data[0] != controlEscape {
		return runeLength(data)
	}

	if len(data) < escapePrefixSize {
		return 0
	}

	switch data[1] {
	case '[', 'O':
		return csiLength(data)

	case ']', 'P', '_', '^', 'X':
		return controlStringLength(data)

	default:
		if !utf8.FullRune(data[1:]) {
			return 0
		}

		_, size := utf8.DecodeRune(data[1:])

		return 1 + size
	}
}

func streamInput(
	ctx context.Context,
	descriptor int,
	events chan<- uv.Event,
) error {
	var (
		decoder eventDecoder
		data    [1024]byte
	)

	last := time.Now()

	for {
		err := decoder.streamStep(ctx, descriptor, events, data[:], &last)
		if err != nil {
			return err
		}
	}
}

func publishEvents(
	ctx context.Context,
	events chan<- uv.Event,
	decoded []uv.Event,
) error {
	for _, event := range decoded {
		select {
		case events <- event:
		case <-ctx.Done():
			return fmt.Errorf("stream input: %w", ctx.Err())
		}
	}

	return nil
}

func csiLength(data []byte) int {
	for index := 2; index < len(data); index++ {
		if data[index] < 0x40 || data[index] > 0x7e {
			continue
		}

		return csiFinalLength(data, index)
	}

	return 0
}

func csiFinalLength(data []byte, index int) int {
	if data[1] != '[' || index != 2 || data[index] != 'M' {
		return index + 1
	}

	if len(data) < legacyMouseSize {
		return 0
	}

	return legacyMouseSize
}

func controlStringLength(data []byte) int {
	indexEnd := bytes.Index(data[2:], []byte{27, '\\'})
	if data[1] == ']' {
		indexByteEnd := bytes.IndexByte(data[2:], controlBell)
		if indexByteEnd >= 0 && (indexEnd < 0 || indexByteEnd < indexEnd) {
			return indexByteEnd + oscBellOverhead
		}
	}

	if indexEnd >= 0 {
		return indexEnd + stringControlOverhead
	}

	return 0
}

func (decoder *eventDecoder) decodeSequence(
	sequence []byte,
	events []uv.Event,
) ([]uv.Event, error) {
	count, value := decoder.decoder.Decode(sequence)
	if count <= 0 || count > len(sequence) {
		return nil, errors.New("terminal decoder made invalid progress")
	}

	decoder.pending = decoder.pending[count:]
	sequence = sequence[:count]

	if multi, ok := value.(uv.MultiEvent); ok {
		for _, child := range multi {
			events = decoder.appendNormalized(sequence, child, events)
		}

		return events, nil
	}

	return decoder.appendNormalized(sequence, value, events), nil
}

func (decoder *eventDecoder) appendNormalized(
	sequence []byte,
	value uv.Event,
	events []uv.Event,
) []uv.Event {
	event := decodedEvent{value: value}

	decoder.correctAlternateKeys(sequence, &event)
	decoder.correctKittyModifiers(sequence, &event)
	correctLegacyKey(sequence, &event)
	captureGraphicsFeatures(sequence, &event)

	if event.value == nil {
		return events
	}

	return append(events, event.value)
}

func correctLegacyKey(sequence []byte, event *decodedEvent) {
	key, ok := event.value.(uv.KeyPressEvent)
	if !ok {
		return
	}

	escapeKey := len(sequence) >= 3 && sequence[0] == controlEscape &&
		sequence[1] == '['
	modifyOtherKeys := bytes.HasPrefix(sequence, []byte("\x1b[27;")) &&
		sequence[len(sequence)-1] == '~'

	legacyF3 := escapeKey && sequence[len(sequence)-1] == 'R'

	if !escapeKey || modifyOtherKeys || legacyF3 {
		event.value = legacyKeyEvent(key)
	}
}

func (decoder *eventDecoder) streamStep(
	ctx context.Context,
	fd int,
	events chan<- uv.Event,
	data []byte,
	last *time.Time,
) error {
	count, err := pollInput(ctx, fd, data)
	if err != nil {
		return err
	}

	if count > 0 {
		*last = time.Now()
	}

	decoded, err := decoder.feed(
		data[:count],
		time.Since(*last) > 75*time.Millisecond,
	)
	if err != nil {
		return err
	}

	err = publishEvents(ctx, events, decoded)
	if err != nil {
		return err
	}

	return nil
}
