package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"image"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

const usage = `Usage: vncat [host]:port [-R]
  -R  readonly; flags may precede destination
  IPv6: [::1]:5900
  Ctrl-] quits locally; Ctrl-C is sent to VNC.
  Requires Kitty graphics (e.g. Ghostty) and terminal pixel metrics.
  VNC authentication is unencrypted; use a trusted network or SSH tunnel.
`

type arguments struct {
	destination string
	readonly    bool
	help        bool
}

func parseArgs(args []string) (arguments, error) {
	var options arguments
	flags := flag.NewFlagSet("vncat", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.BoolVar(&options.readonly, "R", false, "")
	flags.BoolVar(&options.help, "h", false, "")
	flags.BoolVar(&options.help, "help", false, "")
	var switches, positionals []string
	afterDelimiter := false
	for _, arg := range args {
		if arg == "--" && !afterDelimiter {
			afterDelimiter = true
			continue
		}
		if strings.HasPrefix(arg, "-") && !afterDelimiter {
			switches = append(switches, arg)
		} else {
			positionals = append(positionals, arg)
		}
	}
	if err := flags.Parse(switches); err != nil {
		return options, err
	}
	if options.help {
		return options, nil
	}
	if len(positionals) != 1 {
		return options, errors.New("expected one host:port destination")
	}
	options.destination = positionals[0]
	_, port, err := net.SplitHostPort(options.destination)
	if err != nil {
		return options, fmt.Errorf("invalid destination: %w", err)
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return options, errors.New("destination port must be between 1 and 65535")
	}
	return options, nil
}

func main() {
	options, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "vncat:", err)
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if options.help {
		fmt.Print(usage)
		return
	}
	ctx, stop := signal.NotifyContext(
		context.Background(), os.Interrupt,
		unix.SIGTERM, unix.SIGHUP, unix.SIGQUIT,
	)
	defer stop()
	if err := run(ctx, options); err != nil {
		fmt.Fprintln(os.Stderr, "vncat:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, options arguments) (result error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return errors.New("stdin and stdout must be terminals")
	}
	terminal, err := openTerminal(os.Stdin, os.Stdout)
	if err != nil {
		return err
	}
	defer func() {
		if errors.Is(result, context.Canceled) {
			result = nil
		}
		result = errors.Join(result, terminal.close())
	}()
	conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", options.destination)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", options.destination, err)
	}
	defer conn.Close()
	client := &rfbClient{conn: conn, readonly: options.readonly}
	password := func() ([]byte, error) {
		return readPassword(ctx, int(os.Stdin.Fd()), os.Stdout)
	}
	if err := client.handshake(ctx, password); err != nil {
		return err
	}
	if err := terminal.enableInput(); err != nil {
		return fmt.Errorf("enable terminal input: %w", err)
	}
	resizes := make(chan os.Signal, 1)
	signal.Notify(resizes, unix.SIGWINCH)
	defer signal.Stop(resizes)
	return serve(ctx, client, terminal, resizes)
}

func serve(
	parent context.Context,
	client *rfbClient,
	terminal *terminalSession,
	resizes <-chan os.Signal,
) (result error) {
	ctx, cancel := context.WithCancel(parent)
	frames := make(chan *image.RGBA, 1)
	events := make(chan uv.Event, 16)
	networkDone, inputDone := make(chan error, 1), make(chan error, 1)
	go func() { networkDone <- client.stream(ctx, frames) }()
	go func() { inputDone <- streamInput(ctx, int(terminal.input.Fd()), events) }()
	state := inputState{client: client}
	networkFinished, inputFinished := false, false
	defer func() {
		releaseError := state.release()
		cancel()
		client.conn.Close()
		if !networkFinished {
			<-networkDone
		}
		if !inputFinished {
			<-inputDone
		}
		result = errors.Join(result, releaseError)
	}()
	size, err := readSize(int(terminal.input.Fd()), terminalSize{})
	if err != nil {
		return err
	}
	if _, err := render(terminal.output, nil, size, client.readonly); err != nil {
		return err
	}
	refresh := time.NewTicker(time.Second / 30)
	defer refresh.Stop()
	capabilities := time.NewTimer(2 * time.Second)
	defer capabilities.Stop()
	graphicsReady := false
	cellSizeReported := false
	var frame *image.RGBA
	dirty := false
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-networkDone:
			networkFinished = true
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		case err := <-inputDone:
			inputFinished = true
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return fmt.Errorf("read terminal input: %w", err)
		case <-capabilities.C:
			if !graphicsReady {
				return errors.New("terminal did not confirm Kitty graphics support")
			}
			if size.cellWidth <= 0 || size.cellHeight <= 0 {
				return errors.New("terminal did not report pixel dimensions")
			}
		case <-resizes:
			cellSizeReported = false
			size, err = readSize(int(terminal.input.Fd()), size)
			if err != nil {
				return err
			}
			state.view, err = render(terminal.output, nil, size, client.readonly)
			if err != nil {
				return err
			}
			if _, err := io.WriteString(terminal.output, querySize); err != nil {
				return err
			}
			dirty = true
		case next := <-frames:
			frame, dirty = next, true
		case event := <-events:
			switch event := event.(type) {
			case uv.KittyGraphicsEvent:
				if string(event.Payload) != "OK" {
					return errors.New("terminal rejected Kitty graphics operation")
				}
				if event.Options.ID == 31 {
					graphicsReady = true
				}
			case uv.CellSizeEvent:
				if event.Width > 0 && event.Height > 0 &&
					event.Width <= 1024 && event.Height <= 1024 {
					size.cellWidth, size.cellHeight = event.Width, event.Height
					cellSizeReported = true
					dirty = true
				}
			case uv.PixelSizeEvent:
				if !cellSizeReported && size.columns > 0 && size.rows > 0 &&
					event.Width >= size.columns && event.Height >= size.rows &&
					event.Width <= 65535 && event.Height <= 65535 {
					size.cellWidth, size.cellHeight = event.Width/size.columns, event.Height/size.rows
					dirty = true
				}
			default:
				quit, err := state.handle(event)
				if err != nil {
					return fmt.Errorf("forward remote input: %w", err)
				}
				if quit {
					return nil
				}
			}
		case <-refresh.C:
			if !dirty || frame == nil || !graphicsReady || !size.ready() {
				continue
			}
			view, err := render(terminal.output, frame, size, client.readonly)
			if err != nil {
				return err
			}
			state.view = view
			dirty = false
		}
		state.view.cellWidth = size.cellWidth
		state.view.cellHeight = size.cellHeight
	}
}
