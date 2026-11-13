package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"time"

	"github.com/kovetskiy/docopt-go"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

const usage = `Usage:
  vncat [-R | -X] [--] <destination>
  vncat (-h | --help)

Options:
  -R          Live readonly view.
  -X          Interactive control.
  -h --help   Show this help.

  Destination: [host]:port
  Without -R or -X, draw one image and exit.
  Flags may precede destination; -R and -X are mutually exclusive.
  IPv6: [::1]:5900
  Ctrl-] quits locally; Ctrl-C is sent to VNC in -X mode.
  Requires Kitty graphics (e.g. Ghostty) and terminal pixel metrics.
  VNC authentication is unencrypted; use a trusted network or SSH tunnel.
`

type arguments struct {
	ValueDestination string `docopt:"<destination>"`
	FlagReadonly     bool   `docopt:"-R"`
	FlagControl      bool   `docopt:"-X"`
	FlagHelp         bool   `docopt:"--help"`
}

func parseArgs(args []string) (arguments, error) {
	var options arguments

	if args == nil {
		args = []string{}
	}

	parser := docopt.Parser{HelpHandler: docopt.NoHelpHandler}

	parsed, err := parser.ParseArgs(usage, args, "")
	if err != nil {
		return options, fmt.Errorf("parse arguments: %w", err)
	}

	if parsed == nil {
		options.FlagHelp = true

		return options, nil
	}

	delete(parsed, "--")

	err = parsed.Bind(&options)
	if err != nil {
		return options, fmt.Errorf("bind arguments: %w", err)
	}

	return options, validateDestination(options.ValueDestination)
}

func (options arguments) snapshot() bool {
	return !options.FlagReadonly && !options.FlagControl
}

func validateDestination(destination string) error {
	_, port, err := net.SplitHostPort(destination)
	if err != nil {
		return fmt.Errorf("invalid destination: %w", err)
	}

	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return errors.New("destination port must be between 1 and 65535")
	}

	return nil
}

func main() {
	options, err := parseArgs(os.Args[1:])
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "vncat:", err)
		_, _ = fmt.Fprint(os.Stderr, usage)

		os.Exit(usageExitStatus)
	}

	if options.FlagHelp {
		_, _ = fmt.Fprint(os.Stdout, usage)

		return
	}

	ctx, stop := signal.NotifyContext(
		context.Background(), os.Interrupt,
		unix.SIGTERM, unix.SIGHUP, unix.SIGQUIT,
	)

	err = run(ctx, options)

	stop()

	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "vncat:", err)

		os.Exit(1)
	}
}

func run(ctx context.Context, options arguments) error {
	terminal, err := prepareTerminal(options)
	if err != nil {
		return err
	}

	result := connectSession(ctx, options, terminal)
	if errors.Is(result, context.Canceled) {
		result = nil
	}

	result = errors.Join(result, terminal.close())
	if result != nil {
		return result
	}

	return terminal.snapshot.present(terminal.output)
}

func prepareTerminal(options arguments) (*terminalSession, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) ||
		!term.IsTerminal(int(os.Stdout.Fd())) {
		return nil, errors.New("stdin and stdout must be terminals")
	}

	terminal, err := openTerminal(os.Stdin, os.Stdout)
	if err != nil {
		return nil, err
	}

	if options.snapshot() {
		terminal.snapshot = &snapshotPresentation{}
	}

	if os.Getenv("TMUX") != "" {
		terminal.tmux, err = newTmuxGraphics()
		if err != nil {
			return nil, errors.Join(err, terminal.close())
		}
	}

	return terminal, nil
}

func connectSession(
	ctx context.Context,
	options arguments,
	terminal *terminalSession,
) error {
	dialer := net.Dialer{Timeout: connectSeconds * time.Second}

	conn, err := dialer.DialContext(
		ctx,
		"tcp",
		options.ValueDestination,
	)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", options.ValueDestination, err)
	}

	defer func() { _ = conn.Close() }()

	client := &rfbClient{
		conn: conn, readonly: !options.FlagControl,
		singleFrame: options.snapshot(),
	}
	password := func() ([]byte, error) {
		return readPassword(ctx, int(os.Stdin.Fd()), os.Stdout)
	}

	err = client.handshake(ctx, password)
	if err != nil {
		return err
	}

	err = terminal.enableInput()
	if err != nil {
		return fmt.Errorf("enable terminal input: %w", err)
	}

	resizes := make(chan os.Signal, 1)
	signal.Notify(resizes, unix.SIGWINCH)

	defer signal.Stop(resizes)

	return serve(ctx, client, terminal, resizes)
}
