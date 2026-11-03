package main

import (
	"bytes"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestHashPassword(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"with newline", "correct horse\n"},
		{"without newline", "correct horse"},
		{"with carriage return", "correct horse\r\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := hashPassword(strings.NewReader(tt.input), &out); err != nil {
				t.Fatalf("hashPassword: %v", err)
			}
			hash := strings.TrimSpace(out.String())
			if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte("correct horse")); err != nil {
				t.Errorf("printed hash does not match the password: %v", err)
			}
		})
	}
}

func TestHashPasswordRejectsEmptyInput(t *testing.T) {
	for _, input := range []string{"", "\n"} {
		var out bytes.Buffer
		if err := hashPassword(strings.NewReader(input), &out); err == nil {
			t.Errorf("input %q: expected an error", input)
		}
		if out.Len() != 0 {
			t.Errorf("input %q: printed %q, want nothing", input, out.String())
		}
	}
}

func TestRunCommandUnknown(t *testing.T) {
	err := runCommand([]string{"frobnicate"}, strings.NewReader(""), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "frobnicate") {
		t.Errorf("got %v, want an error naming the command", err)
	}
}
