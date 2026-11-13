package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

func readPassword(
	ctx context.Context,
	descriptor int,
	out io.Writer,
) ([]byte, error) {
	_, promptError := io.WriteString(
		out,
		"\x1b[HVNC password (first 8 bytes used): ",
	)
	if promptError != nil {
		return nil, fmt.Errorf("prompt for password: %w", promptError)
	}

	secret, readError := collectPassword(ctx, descriptor)
	_, clearError := io.WriteString(out, "\x1b[2J\x1b[H")

	result := errors.Join(readError, clearError)
	if result != nil {
		clear(secret)

		return nil, result
	}

	return secret, nil
}

func collectPassword(ctx context.Context, descriptor int) ([]byte, error) {
	secret := make([]byte, 0, passwordCapacity)

	var data [1]byte

	for {
		edit := readPasswordEdit(ctx, descriptor, data[:], secret)
		if edit.err != nil {
			return clearedPassword(secret, edit.err)
		}

		secret = edit.secret
		if edit.done {
			return secret, nil
		}
	}
}

func readPasswordEdit(
	ctx context.Context,
	descriptor int,
	data, secret []byte,
) passwordEdit {
	count, err := pollInput(ctx, descriptor, data)
	if err != nil {
		return passwordEdit{err: err}
	}

	if count == 0 {
		return passwordEdit{secret: secret}
	}

	return editPassword(secret, data[0])
}

func clearedPassword(secret []byte, err error) ([]byte, error) {
	clear(secret)

	return nil, err
}

type passwordEdit struct {
	secret []byte
	done   bool
	err    error
}

func editPassword(secret []byte, input byte) passwordEdit {
	switch input {
	case '\r', '\n':
		return passwordEdit{secret: secret, done: true}

	case controlCancel, controlQuit:
		return passwordEdit{err: context.Canceled}

	case controlBackspace, controlDelete:
		if len(secret) > 0 {
			_, width := utf8.DecodeLastRune(secret)
			clear(secret[len(secret)-width:])

			secret = secret[:len(secret)-width]
		}

		return passwordEdit{secret: secret}

	default:
		if len(secret) >= passwordLimit {
			return passwordEdit{err: errors.New("password too long")}
		}

		return passwordEdit{secret: append(secret, input)}
	}
}
