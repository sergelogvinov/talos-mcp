/*
Copyright 2026 Serge Logvinov.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package secrets

import (
	"fmt"
	"os"

	"golang.org/x/term"
)

// ttyPath is the controlling terminal.
const ttyPath = "/dev/tty"

// TerminalPrompt reads a passphrase from the controlling terminal without
// echo. It never reads stdin, which is the MCP transport in mcp mode.
func TerminalPrompt(prompt string) ([]byte, error) {
	tty, err := os.OpenFile(ttyPath, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("no terminal to prompt for a passphrase: %w", err)
	}
	defer tty.Close() //nolint:errcheck

	fmt.Fprint(tty, prompt)

	p, err := term.ReadPassword(int(tty.Fd())) //nolint:gosec // a file descriptor fits in int
	fmt.Fprintln(tty)

	if err != nil {
		return nil, fmt.Errorf("reading passphrase: %w", err)
	}

	return p, nil
}

// HaveTerminal reports whether the process has a controlling terminal to
// prompt on.
func HaveTerminal() bool {
	tty, err := os.OpenFile(ttyPath, os.O_RDWR, 0)
	if err != nil {
		return false
	}

	tty.Close() //nolint:errcheck

	return true
}
