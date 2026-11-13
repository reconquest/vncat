package main

import (
	"bytes"
	"errors"
	"image"
	"io"
	"math"
	"strings"
	"testing"
	"time"
)

func TestFrameRate(t *testing.T) {
	start := time.Unix(100, 0)
	rate := frameRate{since: start}

	for _, testcase := range []struct {
		elapsed time.Duration
		frames  uint64
		want    float64
	}{
		{0, 0, 0},
		{500 * time.Millisecond, 3, 0},
		{time.Second, 7, 10},
		{3500 * time.Millisecond, 5, 2},
		{4 * time.Second, 1, 2},
		{5 * time.Second, 2, 2},
		{6 * time.Second, 0, 0},
		{10 * time.Second, 0, 0},
	} {
		rate.frames += testcase.frames
		rate.sample(start.Add(testcase.elapsed))

		if math.Abs(rate.value-testcase.want) > 0.000001 {
			t.Fatalf(
				"at %v: got %v, want %v",
				testcase.elapsed,
				rate.value,
				testcase.want,
			)
		}
	}
}

func TestFPSFooterRefresh(t *testing.T) {
	for _, readonly := range []bool{false, true} {
		mode := "interactive"
		if readonly {
			mode = "readonly"
		}

		for _, width := range []int{0, 1, 10, 24, 40, 80} {
			var output bytes.Buffer

			size := terminalSize{width, 5, 8, 16}

			err := refreshStatus(&output, size, readonly, 12.5)
			if err != nil {
				t.Fatal(err)
			}

			text := output.String()
			status := "Ctrl-] quit | " + mode + " | 12.5 FPS"

			if width > 0 &&
				!strings.Contains(
					text,
					status[:min(width, len(status))]+"\x1b[H",
				) {
				t.Fatalf("missing bounded status: %q", text)
			}

			if strings.Contains(text, "\x1b_G") ||
				strings.Contains(text, "\x1b[2J") ||
				!strings.HasPrefix(text, beginSync) ||
				!strings.HasSuffix(text, endSync) {
				t.Fatalf(
					"footer refresh changed image or synchronization: %q",
					text,
				)
			}
		}
	}
}

func TestFPSFooterRefreshFailure(t *testing.T) {
	for _, failure := range []error{nil, io.ErrClosedPipe} {
		var writes []string

		writer := renderWriterFunc(func(data []byte) (int, error) {
			writes = append(writes, string(data))
			if len(writes) == 1 {
				return 4, failure
			}

			return len(data), nil
		})
		err := refreshStatus(writer, terminalSize{40, 5, 8, 16}, false, 10)

		want := io.ErrShortWrite
		if failure != nil {
			want = failure
		}

		if !errors.Is(err, want) || len(writes) != 2 ||
			writes[1] != "\x1b\\"+endSync {
			t.Fatal("footer failure did not end synchronization", err, writes)
		}
	}
}

func TestFrameRateCountsPresentations(t *testing.T) {
	rate := frameRate{since: time.Unix(100, 0)}
	frame := image.NewRGBA(image.Rect(0, 0, 2, 2))

	for _, testcase := range []struct {
		frame  *image.RGBA
		size   terminalSize
		writer io.Writer
	}{
		{frame, terminalSize{40, 5, 8, 16}, io.Discard},
		{nil, terminalSize{40, 5, 8, 16}, io.Discard},
		{frame, terminalSize{40, 1, 8, 16}, io.Discard},
		{frame, terminalSize{40, 5, 8, 16}, failedWriter{}},
	} {
		view, err := render(
			testcase.writer,
			testcase.frame,
			testcase.size,
			false,
			rate.value,
		)
		if err == nil && view.width > 0 && view.height > 0 {
			rate.frames++
		}
	}

	err := refreshStatus(
		io.Discard,
		terminalSize{40, 5, 8, 16},
		false,
		rate.value,
	)
	if err != nil {
		t.Fatal(err)
	}

	rate.sample(rate.since.Add(2 * time.Second))

	if rate.value != 0.5 {
		t.Fatal("counted something other than successful images", rate.value)
	}
}
