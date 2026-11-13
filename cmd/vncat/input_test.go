package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
)

func decodeInput(t *testing.T, sequence string) []uv.Event {
	t.Helper()

	var decoder eventDecoder

	events, err := decoder.feed([]byte(sequence), true)
	if err != nil {
		t.Fatal(err)
	}

	return events
}

func keyMessages(t *testing.T, data []byte) []uint32 {
	t.Helper()

	var result []uint32

	for len(data) > 0 {
		if len(data) < 8 || data[0] != 4 {
			t.Fatalf("not key messages: %x", data)
		}

		value := binary.BigEndian.Uint32(data[4:8])
		if data[1] != 0 {
			value |= 0x80000000
		}

		result = append(result, value)
		data = data[8:]
	}

	return result
}

func TestLegacyKeys(t *testing.T) {
	cases := []struct {
		input    string
		symbol   uint32
		modifier uint32
	}{
		{"a", 'a', 0},
		{"A", 'A', 0xffe1},
		{"é", 233, 0},
		{"\u754c", 0x0100754c, 0},
		{"🙂", 0x0101f642, 0},
		{"\x03", 'c', 0xffe3},
		{"\x1bx", 'x', 0xffe9},
		{"\x1b[A", 0xff52, 0},
		{"\x1b[3~", 0xffff, 0},
		{"\x1b[5~", 0xff55, 0},
		{"\x1b[H", 0xff50, 0},
		{"\x1b[F", 0xff57, 0},
		{"\x1bOP", 0xffbe, 0},
		{"\x1b[24~", 0xffc9, 0},
		{"\x1b[1;5D", 0xff51, 0xffe3},
		{"\t", 0xff09, 0},
		{"\r", 0xff0d, 0},
		{"\x7f", 0xff08, 0},
		{"\x1b", 0xff1b, 0},
	}
	for _, testcase := range cases {
		t.Run(testcase.input, func(t *testing.T) {
			conn := newMemoryConn(nil)
			state := inputState{client: &rfbClient{conn: conn}}

			for _, event := range decodeInput(t, testcase.input) {
				quit, err := state.handle(event)
				if quit || err != nil {
					t.Fatal(quit, err)
				}
			}

			want := []uint32{testcase.symbol | 0x80000000, testcase.symbol}
			if testcase.modifier != 0 {
				want = append([]uint32{testcase.modifier | 0x80000000}, want...)
				want = append(want, testcase.modifier)
			}

			got := keyMessages(t, conn.bytes())
			if len(got) != len(want) {
				t.Fatalf("got %x want %x", got, want)
			}

			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("got %x want %x", got, want)
				}
			}
		})
	}
}

func TestKittyHeldKeysAndQuit(t *testing.T) {
	conn := newMemoryConn(nil)
	state := inputState{client: &rfbClient{conn: conn}}
	sequence := "\x1b[?15u\x1b[97;5:1u\x1b[97;5:2u\x1b[" +
		"97;5:3u\x1b[99;5u\x1b[93;5u"
	quit := false

	for _, event := range decodeInput(t, sequence) {
		var err error

		quit, err = state.handle(event)
		if err != nil {
			t.Fatal(err)
		}
	}

	if !quit || !state.kitty {
		t.Fatal("quit or Kitty negotiation")
	}

	err := state.release()
	if err != nil {
		t.Fatal(err)
	}

	got := keyMessages(t, conn.bytes())
	want := []uint32{
		0x8000ffe3,
		0x80000061,
		0x80000061,
		0x61,
		0x80000063,
		0x63,
		0xffe3,
	}

	if len(got) != len(want) {
		t.Fatalf("got %x want %x", got, want)
	}

	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %x want %x", got, want)
		}
	}

	before := len(conn.bytes())

	err = state.release()
	if err != nil {
		t.Fatal(err)
	}

	if len(conn.bytes()) != before {
		t.Fatal("cleanup not idempotent")
	}
}

func TestKittyShiftReleaseIdentity(t *testing.T) {
	conn := newMemoryConn(nil)
	state := inputState{client: &rfbClient{conn: conn}, kitty: true}

	for _, sequence := range []string{
		"\x1b[97:65;2u",
		"\x1b[57441;1:3u",
		"\x1b[97;1:3u",
	} {
		for _, event := range decodeInput(t, sequence) {
			requireInput(t, &state, event)
		}
	}

	got := keyMessages(t, conn.bytes())
	want := []uint32{0x8000ffe1, 0x80000041, 0xffe1, 0x41}

	if len(got) != len(want) {
		t.Fatalf("got %x", got)
	}

	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %x want %x", got, want)
		}
	}
}

func TestInputFragmentationAndBounds(t *testing.T) {
	sequence := "\u754c\x1b[1;5A\x1b[97;1:3u\x1b[<0;4;5M" +
		"\x1b_Gi=31;OK\x1b\\"
	for split := 0; split <= len(sequence); split++ {
		var decoder eventDecoder

		first, err := decoder.feed([]byte(sequence[:split]), false)
		if err != nil {
			t.Fatal(err)
		}

		last, err := decoder.feed([]byte(sequence[split:]), false)
		if err != nil {
			t.Fatal(err)
		}

		if len(first)+len(last) != 5 || len(decoder.pending) != 0 {
			t.Fatalf("split %d: %v %v %q", split, first, last, decoder.pending)
		}
	}

	var decoder eventDecoder

	_, feedError := decoder.feed(
		[]byte("\x1b["+strings.Repeat("1", 5000)),
		false,
	)
	if feedError == nil {
		t.Fatal("unbounded input")
	}

	decoder = eventDecoder{}

	events, err := decoder.feed([]byte("\x1b[123"), true)

	if err != nil || len(events) != 0 || len(decoder.pending) != 0 {
		t.Fatal(events, err)
	}
}

func TestMouseMappingDragReleaseWheel(t *testing.T) {
	conn := newMemoryConn(nil)
	state := inputState{
		client: &rfbClient{conn: conn},
		view: viewport{
			width:        100,
			height:       50,
			remoteWidth:  1000,
			remoteHeight: 500,
			cellWidth:    10,
			cellHeight:   10,
		},
	}
	sequence := "\x1b[<0;3;2M\x1b[<32;5;3M\x1b[<0;20;20" +
		"m\x1b[<64;2;2M\x1b[<65;2;2M\x1b[<2;1;1" +
		"M"

	for _, event := range decodeInput(t, sequence) {
		requireInput(t, &state, event)
	}

	releaseError := state.release()
	if releaseError != nil {
		t.Fatal(releaseError)
	}

	data := conn.bytes()
	masks := []byte{1, 1, 0, 8, 0, 16, 0, 4, 0}

	if len(data) != len(masks)*6 {
		t.Fatalf("messages %x", data)
	}

	for i, mask := range masks {
		if data[i*6] != 5 || data[i*6+1] != mask {
			t.Fatalf("mask %d: %x", i, data)
		}
	}

	if binary.BigEndian.Uint16(data[2:]) != 250 ||
		binary.BigEndian.Uint16(data[4:]) != 150 {
		t.Fatal("click mapping")
	}

	if binary.BigEndian.Uint16(data[8:]) != 450 ||
		binary.BigEndian.Uint16(data[10:]) != 250 {
		t.Fatal("drag mapping")
	}

	if binary.BigEndian.Uint16(data[14:]) != 999 ||
		binary.BigEndian.Uint16(data[16:]) != 499 {
		t.Fatal("release clamp")
	}

	count := len(data)

	for _, event := range decodeInput(t, "\x1b[<0;20;20M") {
		requireInput(t, &state, event)
	}

	if len(conn.bytes()) != count {
		t.Fatal("outside click forwarded")
	}
}

func TestReadonlyIsolation(t *testing.T) {
	conn := newMemoryConn(nil)
	client := &rfbClient{conn: conn, readonly: true}
	state := inputState{
		client:    client,
		kitty:     true,
		held:      map[rune]uint32{'x': 'x'},
		modifiers: uv.ModCtrl,
		buttons:   1,
		view: viewport{
			width:        100,
			height:       100,
			remoteWidth:  100,
			remoteHeight: 100,
			cellWidth:    10,
			cellHeight:   10,
		},
	}

	sequence := "a\x03\x1b[97;1:3u\x1b[<0;1;1M\x1b[<32;" +
		"2;2M\x1b[<0;2;2m\x1b[<64;1;1M"
	for _, event := range decodeInput(t, sequence) {
		requireInput(t, &state, event)
	}

	keyError := client.key('x', true)
	if keyError != nil {
		t.Fatal(keyError)
	}

	pointerError := client.pointer(1, 0, 0)
	if pointerError != nil {
		t.Fatal(pointerError)
	}

	releaseError := state.release()
	if releaseError != nil {
		t.Fatal(releaseError)
	}

	if !bytes.Equal(conn.bytes(), nil) {
		t.Fatalf("readonly sent %x", conn.bytes())
	}

	events := decodeInput(t, "\x1d")
	quit, err := state.handle(events[0])

	if !quit || err != nil {
		t.Fatal("readonly cannot quit", err)
	}
}

func TestLegacyTextWithKittyEnabled(t *testing.T) {
	conn := newMemoryConn(nil)
	state := inputState{client: &rfbClient{conn: conn}, kitty: true}

	for _, event := range decodeInput(t, "a\u754c\x03") {
		requireInput(t, &state, event)
	}

	if len(state.held) != 0 || state.modifiers != 0 {
		t.Fatal("legacy text left held input")
	}

	got := keyMessages(t, conn.bytes())
	want := []uint32{
		0x80000061,
		0x61,
		0x8100754c,
		0x0100754c,
		0x8000ffe3,
		0x80000063,
		0x63,
		0xffe3,
	}

	if len(got) != len(want) {
		t.Fatalf("got %x", got)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %x want %x", got, want)
		}
	}
}

func TestHeldInputFailureAndLimit(t *testing.T) {
	conn := newMemoryConn(nil)
	state := inputState{client: &rfbClient{conn: conn}, kitty: true}

	_, handleError := state.handle(
		uv.KeyPressEvent{Code: 'a', Mod: uv.ModCtrl},
	)
	if handleError != nil {
		t.Fatal(handleError)
	}

	conn.failure = io.ErrClosedPipe
	_, handleError = state.handle(
		uv.KeyReleaseEvent{Code: 'a', Mod: uv.ModCtrl},
	)

	if !errors.Is(
		handleError,
		io.ErrClosedPipe,
	) {
		t.Fatal(handleError)
	}

	if state.held['a'] != 'a' {
		t.Fatal("failed release forgot held key")
	}

	conn.failure = nil

	releaseError := state.release()
	if releaseError != nil {
		t.Fatal(releaseError)
	}

	if len(state.held) != 0 || state.modifiers != 0 {
		t.Fatal("cleanup state")
	}

	state.held = make(map[rune]uint32)
	for i := range 256 {
		state.held[rune(i+32)] = uint32(i + 32)
	}

	_, handleError = state.handle(uv.KeyPressEvent{Code: 'a'})
	if handleError != nil {
		t.Fatal("repeat at limit", handleError)
	}

	_, handleError = state.handle(uv.KeyPressEvent{Code: '\u754c'})
	if handleError == nil {
		t.Fatal("unbounded held keys")
	}
}

func TestPartialCellHit(t *testing.T) {
	view := viewport{
		width:        12,
		height:       12,
		remoteWidth:  120,
		remoteHeight: 120,
		cellWidth:    10,
		cellHeight:   10,
	}
	point, initialInside := view.point(1, 1)

	if !initialInside || point.x != 119 || point.y != 119 {
		t.Fatal(point, initialInside)
	}

	_, pointInside := view.point(2, 1)
	if pointInside {
		t.Fatal("outside cell accepted")
	}
}

func FuzzTerminalDecoder(f *testing.F) {
	for _, sequence := range []string{
		"a\u754c",
		"\x1b]10;rgb:ffff/0000/0000\a\x1b_Gi=31;OK\x1b\\",
		"\x1b\x1b_Gi=31;OK\x1b\\z",
		"\x1b\x1b[A\x1b\x1bOA",
		strings.Repeat("\x1b", sequenceLimit+1),
		"\x1b[97;5:3u",
		"\x1b[<0;2;3M",
		"\x1b_Gi=31;OK\x1b\\",
		"\x1b[999999999999999999999999u",
	} {
		f.Add([]byte(sequence))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 8192 {
			t.Skip()
		}

		var decoder eventDecoder

		for len(data) > 0 {
			size := min(17, len(data))

			_, feedError := decoder.feed(data[:size], false)
			if feedError != nil {
				return
			}

			data = data[size:]
		}

		_, feedError := decoder.feed(nil, true)
		if feedError != nil {
			return
		}

		if len(decoder.pending) != 0 {
			t.Fatal("pending data after timeout")
		}
	})
}

func TestKittyMousePreservesKeyboardModifiers(t *testing.T) {
	for _, testcase := range []struct {
		code   string
		mod    uv.KeyMod
		symbol uint32
	}{
		{"57444", uv.ModSuper, 0xffeb},
		{"57445", uv.ModHyper, 0xffed},
		{"57446", uv.ModMeta, 0xffe7},
	} {
		for _, cleanup := range []bool{false, true} {
			conn := newMemoryConn(nil)
			state := inputState{
				client: &rfbClient{conn: conn},
				kitty:  true,
				view: viewport{
					width:        100,
					height:       50,
					remoteWidth:  1000,
					remoteHeight: 500,
					cellWidth:    10,
					cellHeight:   10,
				},
			}
			sequence := "\x1b[" + testcase.code +
				";1u\x1b[<35;2;2M\x1b[<0;2;2M\x1b[<0;2;2m"

			for _, event := range decodeInput(t, sequence) {
				requireInput(t, &state, event)
			}

			if state.modifiers != testcase.mod {
				t.Fatalf("mouse changed keyboard modifier: %v", state.modifiers)
			}

			if cleanup {
				err := state.release()
				if err != nil {
					t.Fatal(err)
				}
			} else {
				release := "\x1b[" + testcase.code + ";1:3u"
				for _, event := range decodeInput(t, release) {
					requireInput(t, &state, event)
				}
			}

			data := conn.bytes()
			if len(data) != 8+3*6+8 || state.modifiers != 0 {
				t.Fatalf("unexpected modifier transitions: %x", data)
			}

			keyMessagesGot := keyMessages(
				t,
				append(bytes.Clone(data[:8]), data[26:]...),
			)
			if len(keyMessagesGot) != 2 ||
				keyMessagesGot[0] != testcase.symbol|0x80000000 ||
				keyMessagesGot[1] != testcase.symbol {
				t.Fatalf("modifier messages: %x", keyMessagesGot)
			}

			for i, mask := range []byte{0, 1, 0} {
				if data[8+i*6] != 5 || data[9+i*6] != mask {
					t.Fatalf("mouse messages: %x", data)
				}
			}
		}
	}
}

func TestMouseReleaseWithoutViewport(t *testing.T) {
	for _, testcase := range []struct {
		name    string
		press   string
		release string
		mask    byte
	}{
		{"left", "\x1b[<0;3;2M", "\x1b[<0;20;20m", 0},
		{"right", "\x1b[<2;3;2M", "\x1b[<2;20;20m", 0},
		{"all", "\x1b[<0;3;2M\x1b[<2;3;2M", "\x1b[<3;20;20m", 0},
		{"one-of-two", "\x1b[<0;3;2M\x1b[<2;3;2M", "\x1b[<0;20;20m", 4},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			conn := newMemoryConn(nil)
			view := viewport{
				width:        100,
				height:       50,
				remoteWidth:  1000,
				remoteHeight: 500,
				cellWidth:    10,
				cellHeight:   10,
			}
			state := inputState{client: &rfbClient{conn: conn}, view: view}

			for _, event := range decodeInput(t, testcase.press) {
				requireInput(t, &state, event)
			}

			before := len(conn.bytes())

			state.view = viewport{}

			sequence := "\x1b[<1;20;20M\x1b[<32;20;20M\x1b[<64;20;20M"
			for _, event := range decodeInput(t, sequence) {
				requireInput(t, &state, event)
			}

			if len(conn.bytes()) != before {
				t.Fatal("input forwarded without viewport")
			}

			for _, event := range decodeInput(t, testcase.release) {
				requireInput(t, &state, event)
			}

			want := make([]byte, 0, 12)

			want = append(want, 5, testcase.mask, 0, 250, 0, 150)

			if got := conn.bytes()[before:]; !bytes.Equal(got, want) {
				t.Fatalf("release got %x want %x", got, want)
			}

			state.view = view
			for _, event := range decodeInput(t, "\x1b[<35;5;3M") {
				requireInput(t, &state, event)
			}

			want = append(want, 5, testcase.mask, 1, 194, 0, 250)
			if got := conn.bytes()[before:]; !bytes.Equal(got, want) {
				t.Fatalf("restored motion got %x want %x", got, want)
			}
		})
	}
}

func TestKittyBothModifierSides(t *testing.T) {
	for _, testcase := range []struct {
		name        string
		left, right rune
		mod         uv.KeyMod
		symbol      uint32
	}{
		{"shift", uv.KeyLeftShift, uv.KeyRightShift, uv.ModShift, 0xffe1},
		{"ctrl", uv.KeyLeftCtrl, uv.KeyRightCtrl, uv.ModCtrl, 0xffe3},
		{"alt", uv.KeyLeftAlt, uv.KeyRightAlt, uv.ModAlt, 0xffe9},
		{"super", uv.KeyLeftSuper, uv.KeyRightSuper, uv.ModSuper, 0xffeb},
		{"hyper", uv.KeyLeftHyper, uv.KeyRightHyper, uv.ModHyper, 0xffed},
		{"meta", uv.KeyLeftMeta, uv.KeyRightMeta, uv.ModMeta, 0xffe7},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			for _, rightFirst := range []bool{false, true} {
				for _, cleanup := range []bool{false, true} {
					conn := newMemoryConn(nil)
					state := inputState{
						client: &rfbClient{conn: conn},
						kitty:  true,
						view: viewport{
							width:        100,
							height:       50,
							remoteWidth:  1000,
							remoteHeight: 500,
							cellWidth:    10,
							cellHeight:   10,
						},
					}
					first, last := testcase.left, testcase.right

					if rightFirst {
						first, last = last, first
					}

					for _, event := range []uv.Event{
						uv.KeyPressEvent{Code: first},
						uv.KeyPressEvent{Code: last, Mod: testcase.mod},
						uv.KeyPressEvent{
							Code: last, Mod: testcase.mod, IsRepeat: true,
						},
						uv.KeyReleaseEvent{Code: first, Mod: testcase.mod},
					} {
						requireInput(t, &state, event)
					}

					got := keyMessages(t, conn.bytes())
					if len(got) != 1 || got[0] != testcase.symbol|0x80000000 {
						t.Fatalf(
							"one-side release changed aggregate modifier: %x",
							got,
						)
					}

					sequence := "\x1b[<35;2;2M\x1b[<0;2;2M\x1b[<0;2;2m"
					for _, event := range decodeInput(t, sequence) {
						requireInput(t, &state, event)
					}

					if len(conn.bytes()) != 8+3*6 ||
						state.modifiers != testcase.mod {
						t.Fatalf(
							"mouse changed held modifier: %x",
							conn.bytes(),
						)
					}

					for _, event := range decodeInput(t, "x") {
						requireInput(t, &state, event)
					}

					if state.modifiers != testcase.mod {
						t.Fatal("legacy snapshot cleared held modifier")
					}

					if !cleanup {
						_, handleError := state.handle(
							uv.KeyReleaseEvent{Code: last},
						)
						if handleError != nil {
							t.Fatal(handleError)
						}
					}

					err := state.release()
					if err != nil {
						t.Fatal(err)
					}

					data := conn.bytes()

					got = keyMessages(t, data[26:])

					want := []uint32{0x80000078, 0x78, testcase.symbol}

					if !slices.Equal(got, want) || state.modifiers != 0 {
						t.Fatalf("final release got %x want %x", got, want)
					}

					err = state.syncModifiers(0)
					if err != nil {
						t.Fatal(err)
					}

					err = state.release()
					if err != nil {
						t.Fatal(err)
					}

					if !bytes.Equal(conn.bytes(), data) {
						t.Fatal("cleanup retained physical modifiers")
					}
				}
			}
		})
	}
}

func TestKittyThreeAlternateKeyFields(t *testing.T) {
	for _, testcase := range []struct {
		sequence string
		shifted  rune
		text     string
		symbol   uint32
	}{
		{"\x1b[97:65:113;2u", 'A', "A", 'A'},
		{"\x1b[97::113;2u", 0, "A", 'A'},
		{"\x1b[97:65:113u", 'A', "a", 'a'},
		{"\x1b[97:65:113;2;90u", 'A', "Z", 'A'},
	} {
		t.Run(testcase.sequence, func(t *testing.T) {
			sequence := testcase.sequence + testcase.sequence
			for split := 0; split <= len(sequence); split++ {
				var decoder eventDecoder

				events, decodeError := decoder.feed(
					[]byte(sequence[:split]),
					false,
				)
				if decodeError != nil {
					t.Fatal(decodeError)
				}

				last, decodeError := decoder.feed(
					[]byte(sequence[split:]),
					false,
				)
				if decodeError != nil {
					t.Fatal(decodeError)
				}

				events = append(events, last...)
				if len(events) != 2 {
					t.Fatalf("split %d: %v", split, events)
				}

				conn := newMemoryConn(nil)
				state := inputState{client: &rfbClient{conn: conn}, kitty: true}

				for _, event := range events {
					key, ok := event.(uv.KeyPressEvent)
					if !ok || key.Code != 'a' ||
						key.ShiftedCode != testcase.shifted ||
						key.BaseCode != 'q' ||
						key.Text != testcase.text {
						t.Fatalf("incorrect alternate fields: %#v", event)
					}

					requireInput(t, &state, event)
				}

				for _, event := range decodeInput(t, "\x1b[97:65:113;1:3u") {
					key, ok := event.(uv.KeyReleaseEvent)
					if !ok || key.ShiftedCode != 'A' || key.BaseCode != 'q' {
						t.Fatalf("incorrect release fields: %#v", event)
					}

					requireInput(t, &state, event)
				}

				want := []uint32{
					testcase.symbol | 0x80000000,
					testcase.symbol | 0x80000000,
					testcase.symbol,
				}
				firstKey, ok := events[0].(uv.KeyPressEvent)

				if !ok {
					t.Fatal("first event is not a key press")
				}

				if firstKey.Mod.Contains(uv.ModShift) {
					want = []uint32{
						0x8000ffe1,
						testcase.symbol | 0x80000000,
						testcase.symbol | 0x80000000,
						0xffe1,
						testcase.symbol,
					}
				}

				keyMessagesGot := keyMessages(
					t,
					conn.bytes(),
				)
				if !slices.Equal(
					keyMessagesGot,
					want,
				) {
					t.Fatalf("got %x want %x", keyMessagesGot, want)
				}
			}
		})
	}
}

func TestKittyNavigationModifiers(t *testing.T) {
	for _, modifier := range []struct {
		name     string
		value    int
		physical int
		symbol   uint32
	}{
		{"super", 9, 57444, 0xffeb},
		{"meta", 33, 57446, 0xffe7},
	} {
		for _, key := range []struct {
			name   string
			code   int
			final  byte
			symbol uint32
		}{
			{"up", 1, 'A', 0xff52},
			{"begin", 1, 'E', 0xff58},
			{"f1", 1, 'P', 0xffbe},
			{"delete", 3, '~', 0xffff},
			{"f5", 15, '~', 0xffc2},
		} {
			for _, physical := range []bool{false, true} {
				for _, cleanup := range []bool{false, true} {
					for _, explicit := range []bool{false, true} {
						name := fmt.Sprintf(
							"%s/%s/physical=%t/cleanup=%t/explicit=%t",
							modifier.name,
							key.name,
							physical,
							cleanup,
							explicit,
						)
						t.Run(name, func(t *testing.T) {
							sequence := "\x1b[?15u"
							if physical {
								sequence += fmt.Sprintf(
									"\x1b[%d;1u",
									modifier.physical,
								)
							}

							eventType := ""
							if explicit {
								eventType = ":1"
							}

							sequence += fmt.Sprintf(
								"\x1b[%d;%d%s%c\x1b[%d;%d:2%c",
								key.code,
								modifier.value,
								eventType,
								key.final,
								key.code,
								modifier.value,
								key.final,
							)
							if !cleanup {
								sequence += fmt.Sprintf(
									"\x1b[%d;%d:3%c",
									key.code,
									modifier.value,
									key.final,
								)
								if physical {
									sequence += fmt.Sprintf(
										"\x1b[%d;1:3u",
										modifier.physical,
									)
								}
							}

							for split := 0; split <= len(sequence); split++ {
								var decoder eventDecoder

								conn := newMemoryConn(nil)
								state := inputState{
									client: &rfbClient{conn: conn},
								}

								pieces := []string{
									sequence[:split],
									sequence[split:],
								}
								for _, piece := range pieces {
									events, err := decoder.feed(
										[]byte(piece),
										false,
									)
									if err != nil {
										t.Fatal(err)
									}

									for _, event := range events {
										handleQuit, handleError := state.handle(
											event,
										)
										if handleQuit ||
											handleError != nil {
											t.Fatal(handleQuit, handleError)
										}
									}
								}

								err := state.release()
								if err != nil {
									t.Fatal(err)
								}

								want := []uint32{
									modifier.symbol | 0x80000000,
									key.symbol | 0x80000000,
									key.symbol | 0x80000000,
									key.symbol,
									modifier.symbol,
								}
								keyMessagesGot := keyMessages(
									t,
									conn.bytes(),
								)

								if !slices.Equal(
									keyMessagesGot,
									want,
								) {
									t.Fatalf(
										"split %d: got %x want %x",
										split,
										keyMessagesGot,
										want,
									)
								}

								if len(state.held) != 0 ||
									len(state.heldModifiers) != 0 ||
									state.modifiers != 0 {
									t.Fatal("cleanup retained held input")
								}

								before := conn.bytes()

								err = state.release()

								if err != nil ||
									!bytes.Equal(before, conn.bytes()) {
									t.Fatal("cleanup not idempotent", err)
								}
							}
						})
					}
				}
			}
		}
	}
}

func TestDecodedModifierSemantics(t *testing.T) {
	for _, testcase := range []struct {
		sequence string
		mod      uv.KeyMod
	}{
		{"\x1b[1;9A", uv.ModMeta},
		{"\x1b[15;33~", uv.ModSuper},
		{"\x1b[1;9:1A", uv.ModSuper},
		{"\x1b[1;9:1Z", uv.ModShift | uv.ModSuper},
		{"\x1b[15;33:3~", uv.ModMeta},
		{
			"\x1b[1;256:2A",
			uv.ModShift | uv.ModAlt | uv.ModCtrl | uv.ModSuper |
				uv.ModHyper | uv.ModMeta | uv.ModCapsLock | uv.ModNumLock,
		},
		{"\x1b[1;16:1A", uv.ModShift | uv.ModAlt | uv.ModCtrl | uv.ModSuper},
		{"\x1b[15;49:3~", uv.ModHyper | uv.ModMeta},
		{"\x1b[?15u\x1b[97;9u", uv.ModSuper},
		{"\x1b[?15u\x1b[97;33:3u", uv.ModMeta},
		{"\x1b[?15u\x1b[27;9;97~", uv.ModMeta},
		{"\x1b[?15u\x1b[?0u\x1b[1;9A", uv.ModMeta},
	} {
		t.Run(testcase.sequence, func(t *testing.T) {
			events := decodeInput(t, testcase.sequence)
			if len(events) == 0 {
				t.Fatal("no decoded events")
			}

			event := events[len(events)-1]
			if legacy, ok := event.(legacyKeyEvent); ok {
				event = uv.KeyPressEvent(legacy)
			}

			key, ok := event.(uv.KeyEvent)
			if !ok || key.Key().Mod != testcase.mod {
				t.Fatalf("got %#v want modifiers %v", events, testcase.mod)
			}
		})
	}
}

func TestKittyKeypadBeginAndSeparator(t *testing.T) {
	for _, testcase := range []struct {
		name   string
		code   int
		mod    int
		symbol uint32
	}{
		{"begin", 57427, 1, 0xff9d},
		{"ctrl-begin", 57427, 5, 0xff9d},
		{"separator", 57416, 1, 0xffac},
		{"ctrl-separator", 57416, 5, 0xffac},
	} {
		for _, cleanup := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("%s/cleanup=%t", testcase.name, cleanup),
				func(t *testing.T) {
					conn := newMemoryConn(nil)
					state := inputState{client: &rfbClient{conn: conn}}
					sequence := fmt.Sprintf(
						"\x1b[?15u\x1b[%d;%du\x1b[%d;%d:2u",
						testcase.code,
						testcase.mod,
						testcase.code,
						testcase.mod,
					)
					want := []uint32{
						testcase.symbol | 0x80000000,
						testcase.symbol | 0x80000000,
					}

					if testcase.mod == 5 {
						want = append([]uint32{0x8000ffe3}, want...)
					}

					if !cleanup {
						sequence += fmt.Sprintf(
							"\x1b[%d;%d:3u",
							testcase.code,
							testcase.mod,
						)

						want = append(want, testcase.symbol)
					}

					for _, event := range decodeInput(t, sequence) {
						handleQuit, handleError := state.handle(
							event,
						)
						if handleQuit ||
							handleError != nil {
							t.Fatal(handleQuit, handleError)
						}
					}

					keyMessagesGot := keyMessages(
						t,
						conn.bytes(),
					)
					if !slices.Equal(
						keyMessagesGot,
						want,
					) {
						t.Fatalf("got %x want %x", keyMessagesGot, want)
					}

					err := state.release()
					if err != nil {
						t.Fatal(err)
					}

					if cleanup {
						want = append(want, testcase.symbol)
					}

					if testcase.mod == 5 {
						want = append(want, 0xffe3)
					}

					keyMessagesGot = keyMessages(
						t,
						conn.bytes(),
					)
					if !slices.Equal(
						keyMessagesGot,
						want,
					) {
						t.Fatalf("cleanup got %x want %x", keyMessagesGot, want)
					}

					if len(state.held) != 0 || len(state.heldModifiers) != 0 ||
						state.modifiers != 0 {
						t.Fatal("cleanup retained held input")
					}

					before := conn.bytes()

					err = state.release()

					if err != nil || !bytes.Equal(before, conn.bytes()) {
						t.Fatal("cleanup not idempotent", err)
					}
				},
			)
		}
	}
}

func TestKeypadCommaSymbol(t *testing.T) {
	keySymbolGot := keySymbol(uv.Key{Code: uv.KeyKpComma, Text: ","})
	if keySymbolGot != 0xffac {
		t.Fatalf("got %x want ffac", keySymbolGot)
	}
}

func TestUnsupportedMouseReleasePreservesDrag(t *testing.T) {
	for _, release := range []string{"\x1b[<128;1;1m", "\x1b[<129;1;1m"} {
		conn := newMemoryConn(nil)
		state := inputState{
			client: &rfbClient{conn: conn},
			view: viewport{
				width: 100, height: 50,
				remoteWidth: 100, remoteHeight: 50,
				cellWidth: 10, cellHeight: 10,
			},
		}
		sequence := "\x1b[<0;1;1M" + release + "\x1b[<32;2;2M" +
			"\x1b[<0;2;2m"

		for _, event := range decodeInput(t, sequence) {
			requireInput(t, &state, event)
		}

		data := conn.bytes()
		if len(data) != 24 {
			t.Fatalf("unexpected mouse output: %x", data)
		}

		for index, mask := range []byte{1, 1, 1, 0} {
			if data[index*6+1] != mask {
				t.Fatalf("drag interrupted: %x", data)
			}
		}
	}
}
