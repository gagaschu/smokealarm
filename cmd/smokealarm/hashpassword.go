package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/gagaschu/smokealarm/internal/auth"
)

// runCommand handles the subcommands. There is only one, so a plain switch
// is enough; no command-line framework needed.
func runCommand(args []string, stdin io.Reader, stdout io.Writer) error {
	switch args[0] {
	case "hash-password":
		return hashPassword(stdin, stdout)
	default:
		return fmt.Errorf("unknown command %q; usage: smokealarm [hash-password]", args[0])
	}
}

// hashPassword reads one line from stdin and prints its bcrypt hash. Reading
// from stdin keeps the password out of the process list and shell history:
//
//	read -s PW; printf '%s' "$PW" | smokealarm hash-password
func hashPassword(stdin io.Reader, stdout io.Writer) error {
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read password: %w", err)
	}
	password := strings.TrimRight(line, "\r\n")
	if password == "" {
		return errors.New("no password given on stdin")
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(stdout, hash); err != nil {
		return fmt.Errorf("write hash: %w", err)
	}
	return nil
}
