package main

import (
	"bytes"
	"fmt"
	"strconv"

	uv "github.com/charmbracelet/ultraviolet"
)

type pixelMouseEvent struct {
	event uv.MouseEvent
	x, y  int
}

type sgrMouseReport struct {
	button uint64
	x, y   int
	final  byte
}

const (
	sgrMouseFields = 3
	sgrMousePrefix = "\x1b[<"
)

func sgrMouseSequence(sequence []byte) bool {
	return bytes.HasPrefix(sequence, []byte(sgrMousePrefix))
}

func (decoder *eventDecoder) discardInterruptedMouse() {
	for {
		interruption := sgrMouseInterruption(decoder.pending)
		if interruption == 0 {
			return
		}

		decoder.pending = decoder.pending[interruption:]
	}
}

func sgrMouseInterruption(sequence []byte) int {
	if !sgrMouseSequence(sequence) {
		return 0
	}

	limit := min(len(sequence), sequenceLimit+1)
	for index := len(sgrMousePrefix); index < limit; index++ {
		value := sequence[index]
		if value >= '0' && value <= '9' {
			continue
		}

		switch value {
		case 'M', 'm':
			return 0

		case '-', ';':
			continue

		default:
			return index
		}
	}

	return 0
}

func (decoder *eventDecoder) appendSGRMouse(
	sequence []byte,
	events []uv.Event,
) []uv.Event {
	decoder.pending = decoder.pending[len(sequence):]

	event := decoder.decodeSGRMouse(sequence)
	if event.value != nil {
		return append(events, event.value)
	}

	return events
}

func (decoder *eventDecoder) decodeSGRMouse(sequence []byte) decodedEvent {
	report, valid := parseSGRMouse(sequence)
	if !valid {
		return decodedEvent{}
	}

	column, row := report.x, report.y
	if decoder.mousePixels {
		column, row = 1, 1
	} else if column < 0 || row < 0 {
		return decodedEvent{}
	}

	normalized := fmt.Sprintf(
		"\x1b[<%d;%d;%d%c", report.button, column, row, report.final,
	)
	_, value := decoder.decoder.Decode([]byte(normalized))

	mouse, ok := value.(uv.MouseEvent)
	if !ok {
		return decodedEvent{}
	}

	if decoder.mousePixels {
		return decodedEvent{
			value: pixelMouseEvent{event: mouse, x: report.x, y: report.y},
		}
	}

	return decodedEvent{value: mouse}
}

func parseSGRMouse(sequence []byte) (sgrMouseReport, bool) {
	body := bytes.TrimLeft(sequence, "\x1b")
	if len(body) <= len("[<") {
		return sgrMouseReport{}, false
	}

	final := body[len(body)-1]
	if final != 'M' && final != 'm' {
		return sgrMouseReport{}, false
	}

	fields := bytes.Split(body[2:len(body)-1], []byte{';'})
	if len(fields) != sgrMouseFields {
		return sgrMouseReport{}, false
	}

	button, err := strconv.ParseUint(string(fields[0]), 10, 8)
	if err != nil {
		return sgrMouseReport{}, false
	}

	x, xValid := mouseCoordinate(fields[1])
	y, yValid := mouseCoordinate(fields[2])

	return sgrMouseReport{
		button: button, x: x, y: y, final: final,
	}, xValid && yValid
}

func mouseCoordinate(field []byte) (int, bool) {
	if len(field) == 0 || field[0] == '+' {
		return 0, false
	}

	coordinate, err := strconv.ParseInt(string(field), 10, 32)

	return int(coordinate), err == nil
}
