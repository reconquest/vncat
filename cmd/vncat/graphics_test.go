package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math/rand"
	"strings"
	"testing"
)

type failedWriter struct{}

func (failedWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestGraphicsPNGChunksAndAspect(t *testing.T) {
	frame := image.NewRGBA(image.Rect(0, 0, 320, 160))
	random := rand.New(rand.NewSource(1))
	for y := 0; y < 160; y++ {
		for x := 0; x < 320; x++ {
			frame.SetRGBA(x, y, color.RGBA{uint8(random.Intn(256)), uint8(random.Intn(256)), uint8(random.Intn(256)), 255})
		}
	}
	var output bytes.Buffer
	view, err := render(&output, frame, terminalSize{columns: 40, rows: 15, cellWidth: 8, cellHeight: 16}, false)
	if err != nil {
		t.Fatal(err)
	}
	if view.width != 320 || view.height != 160 {
		t.Fatal(view)
	}
	if !strings.Contains(output.String(), "a=T") || !strings.Contains(output.String(), "C=1") || strings.Contains(output.String(), ",c=") || strings.Contains(output.String(), ",r=") {
		t.Fatal("not native pixel placement")
	}
	var encoded strings.Builder
	chunks := 0
	for _, part := range strings.Split(output.String(), "\x1b_G")[1:] {
		command := strings.SplitN(part, "\x1b\\", 2)[0]
		pieces := strings.SplitN(command, ";", 2)
		if len(pieces) != 2 {
			continue
		}
		if len(pieces[1]) > 4096 || len(pieces[1])%4 != 0 {
			t.Fatal("invalid chunk size")
		}
		encoded.WriteString(pieces[1])
		chunks++
	}
	if chunks < 2 || !strings.Contains(output.String(), "m=1") || !strings.Contains(output.String(), "m=0") {
		t.Fatal("not chunked")
	}
	data, err := base64.StdEncoding.DecodeString(encoded.String())
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Bounds() != frame.Rect || decoded.At(8, 9) != frame.At(8, 9) {
		t.Fatal("pixel payload mismatch")
	}
	output.Reset()
	frame.SetRGBA(8, 9, color.RGBA{255, 0, 0, 255})
	if _, err := render(&output, frame, terminalSize{columns: 10, rows: 20, cellWidth: 8, cellHeight: 16}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := render(failedWriter{}, frame, terminalSize{columns: 1, rows: 1, cellWidth: 8, cellHeight: 16}, false); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
}

func TestFitAndPointerGeometry(t *testing.T) {
	for _, tc := range []struct {
		frame         image.Rectangle
		size          terminalSize
		width, height int
	}{
		{image.Rect(0, 0, 1920, 1080), terminalSize{80, 24, 10, 20}, 800, 450},
		{image.Rect(0, 0, 800, 1200), terminalSize{80, 24, 10, 20}, 306, 460},
		{image.Rect(0, 0, 2, 1), terminalSize{8, 2, 10, 20}, 40, 20},
	} {
		view, err := fit(tc.frame, tc.size)
		if err != nil || view.width != tc.width || view.height != tc.height {
			t.Fatal(view, err)
		}
		x, y, inside := view.point(0, 0)
		if !inside || int(x) != tc.size.cellWidth/2*tc.frame.Dx()/tc.width || int(y) != tc.size.cellHeight/2*tc.frame.Dy()/tc.height {
			t.Fatal(x, y, inside)
		}
	}
	for _, size := range []terminalSize{{}, {80, 24, 0, 0}, {80, 1, 8, 16}, {65535, 65535, 1000, 1000}} {
		if _, err := fit(image.Rect(0, 0, 1, 1), size); err == nil {
			t.Fatal("accepted invalid metrics")
		}
	}
}

func TestCLI(t *testing.T) {
	for _, args := range [][]string{{"host:5900"}, {"-R", "host:5900"}, {"host:5900", "-R"}, {"[::1]:5900", "-R"}, {"-R", "[2001:db8::1]:5901"}, {":5900"}} {
		got, err := parseArgs(args)
		if err != nil {
			t.Fatal(args, err)
		}
		if got.readonly != (len(args) > 1) {
			t.Fatal(args, got)
		}
	}
	for _, args := range [][]string{nil, {"host"}, {"host:0"}, {"host:65536"}, {"host:http"}, {"::1:5900"}, {"host:5900", "other:5900"}, {"host:5900", "-unknown"}, {"-R"}} {
		if _, err := parseArgs(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
	for _, arg := range []string{"-h", "--help"} {
		got, err := parseArgs([]string{arg})
		if err != nil || !got.help {
			t.Fatal(got, err)
		}
	}
}

func TestStatusAndReservedViewport(t *testing.T) {
	frame := image.NewRGBA(image.Rect(0, 0, 100, 100))
	for _, readonly := range []bool{false, true} {
		mode := "interactive"
		if readonly {
			mode = "readonly"
		}
		for _, size := range []terminalSize{
			{40, 5, 8, 16}, {32, 4, 10, 20}, {10, 2, 8, 16},
			{1, 1, 8, 16}, {40, 1, 8, 16}, {0, 0, 8, 16},
		} {
			var output bytes.Buffer
			view, err := render(&output, frame, size, readonly)
			if err != nil {
				t.Fatal(size, err)
			}
			text := output.String()
			if !strings.HasPrefix(text, "\x1b[?2026h\x1b_Ga=d,d=I,i=1,q=2\x1b\\\x1b[2J\x1b[H") {
				t.Fatal("image or footer cleared outside synchronization")
			}
			if !strings.HasSuffix(text, "\x1b[?2026l") ||
				strings.Count(text, "\x1b[?2026h") != 1 ||
				strings.Count(text, "\x1b[?2026l") != 1 {
				t.Fatal("frame and footer not presented atomically")
			}
			if size.rows > 0 {
				status := "Ctrl-] quit | " + mode
				want := fmt.Sprintf("\x1b[%d;1H\x1b[2K%s\x1b[H", size.rows, status[:min(len(status), size.columns)])
				if !strings.Contains(text, want) {
					t.Fatalf("missing bounded footer %q in %q", want, text)
				}
			}
			if size.rows < 2 {
				if view != (viewport{}) || strings.Contains(text, "a=T") {
					t.Fatal("image displayed without available rows", view)
				}
				continue
			}
			if view.width != view.height || view.height > (size.rows-1)*size.cellHeight {
				t.Fatal("aspect ratio or footer reservation lost", view)
			}
			if _, _, inside := view.point(0, size.rows-1); inside {
				t.Fatal("footer mapped to desktop")
			}
			if _, _, inside := view.point(0, size.rows-2); !inside {
				t.Fatal("last image row not mapped to desktop")
			}
		}
		var output bytes.Buffer
		view, err := render(&output, nil, terminalSize{columns: 40, rows: 5}, readonly)
		if err != nil || view != (viewport{}) || !strings.Contains(output.String(), "Ctrl-] quit | "+mode) {
			t.Fatal("missing footer before first frame or pixel metrics", view, err)
		}
	}
	if err := renderStatus(failedWriter{}, terminalSize{columns: 40, rows: 5}, false); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal("footer output failure lost", err)
	}
}

func TestFooterPointerInput(t *testing.T) {
	view, err := fit(image.Rect(0, 0, 100, 100), terminalSize{40, 5, 8, 16})
	if err != nil {
		t.Fatal(err)
	}
	conn := newMemoryConn(nil)
	state := inputState{client: &rfbClient{conn: conn}, view: view}
	for _, event := range decodeInput(t, "\x1b[<0;1;5M\x1b[<64;1;5M") {
		if _, err := state.handle(event); err != nil {
			t.Fatal(err)
		}
	}
	if len(conn.bytes()) != 0 || state.buttons != 0 {
		t.Fatal("footer click or wheel forwarded")
	}
	for _, event := range decodeInput(t, "\x1b[<0;1;4M\x1b[<0;1;5m") {
		if _, err := state.handle(event); err != nil {
			t.Fatal(err)
		}
	}
	messages := conn.bytes()
	if len(messages) != 12 || messages[1] != 1 || messages[7] != 0 || state.buttons != 0 {
		t.Fatalf("drag release on footer lost: %x", messages)
	}
	if wireOrder.Uint16(messages[4:6]) != 87 || wireOrder.Uint16(messages[10:12]) != 99 {
		t.Fatalf("pointer not mapped to displayed image: %x", messages)
	}
}

type renderWriterFunc func([]byte) (int, error)

func (f renderWriterFunc) Write(data []byte) (int, error) { return f(data) }

func TestRenderPreparedBeforeOutput(t *testing.T) {
	frame := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for i := range frame.Pix {
		frame.Pix[i] = 255
	}
	size := terminalSize{10, 3, 8, 16}
	var expected, output bytes.Buffer
	if _, err := render(&expected, frame, size, false); err != nil {
		t.Fatal(err)
	}
	writer := renderWriterFunc(func(data []byte) (int, error) {
		if output.Len() == 0 && !bytes.Equal(data, bytes.TrimSuffix(expected.Bytes(), []byte(endSync))) {
			t.Fatal("terminal write began without complete replacement")
		}
		clear(frame.Pix)
		return output.Write(data)
	})
	if _, err := render(writer, frame, size, false); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), expected.Bytes()) {
		t.Fatal("frame preparation continued after terminal output began")
	}
	output.Reset()
	if _, err := render(&output, frame, size, false); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(output.Bytes(), expected.Bytes()) {
		t.Fatal("replacement did not include updated pixels")
	}
}

func TestRenderPreparationFailurePreservesDisplay(t *testing.T) {
	for _, tc := range []struct {
		frame *image.RGBA
		size  terminalSize
	}{
		{image.NewRGBA(image.Rect(0, 0, 1, 1)), terminalSize{80, 24, 0, 0}},
		{image.NewRGBA(image.Rect(0, 0, 1, 1)), terminalSize{65535, 65535, 1000, 1000}},
		{image.NewRGBA(image.Rect(0, 0, 1, 1)), terminalSize{8192, 8192, 1, 1}},
		{image.NewRGBA(image.Rectangle{}), terminalSize{80, 24, 8, 16}},
	} {
		var output bytes.Buffer
		view, err := render(&output, tc.frame, tc.size, false)
		if err == nil || view != (viewport{}) || output.Len() != 0 {
			t.Fatal("preparation failure changed display", view, err, output.String())
		}
	}
}

func TestRenderOutputFailureEndsSynchronization(t *testing.T) {
	frame := image.NewRGBA(image.Rect(0, 0, 8, 8))
	size := terminalSize{10, 3, 8, 16}
	var complete bytes.Buffer
	if _, err := render(&complete, frame, size, false); err != nil {
		t.Fatal(err)
	}
	for _, failure := range []error{io.ErrClosedPipe, nil} {
		for _, cut := range []int{
			0, 4, len(beginSync), strings.Index(complete.String(), "Ctrl-]"),
			strings.LastIndex(complete.String(), ";") + 4,
			complete.Len() - len(endSync) - 1,
		} {
			var writes []string
			writer := renderWriterFunc(func(data []byte) (int, error) {
				writes = append(writes, string(data))
				if len(writes) == 1 {
					return cut, failure
				}
				return len(data), nil
			})
			view, err := render(writer, frame, size, false)
			want := failure
			if want == nil {
				want = io.ErrShortWrite
			}
			if !errors.Is(err, want) || view != (viewport{}) {
				t.Fatal("output failure lost or viewport published", cut, view, err)
			}
			if len(writes) != 2 || writes[1] != "\x1b\\\x1b[?2026l" {
				t.Fatal("output failure left synchronization active", cut, writes)
			}
		}
	}
	endFailure := errors.New("end synchronization failed")
	for _, failure := range []error{nil, io.ErrClosedPipe} {
		for _, endError := range []error{nil, endFailure} {
			calls := 0
			writer := renderWriterFunc(func(data []byte) (int, error) {
				calls++
				if calls == 1 {
					return len(data), failure
				}
				return 0, endError
			})
			view, err := render(writer, frame, size, false)
			want := endError
			if want == nil {
				want = io.ErrShortWrite
			}
			if view != (viewport{}) || !errors.Is(err, want) ||
				(failure != nil && !errors.Is(err, failure)) {
				t.Fatal("synchronization error lost or viewport published", view, err)
			}
		}
	}
}
