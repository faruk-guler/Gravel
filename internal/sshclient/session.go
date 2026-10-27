package sshclient

import (
	"fmt"
	"io"

	"golang.org/x/crypto/ssh"
)

// Shell is an interactive PTY session over an established SSH client.
type Shell struct {
	sess *ssh.Session
	in   io.WriteCloser
	out  io.Reader
}

// StartShell requests a PTY of the given size and launches the remote shell.
func StartShell(client *ssh.Client, term string, cols, rows int) (*Shell, error) {
	if client == nil {
		return nil, fmt.Errorf("sshclient: nil client")
	}
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	if term == "" {
		term = "xterm-256color"
	}
	sess, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("sshclient: new session: %w", err)
	}
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := sess.RequestPty(term, rows, cols, modes); err != nil {
		sess.Close()
		return nil, fmt.Errorf("sshclient: request pty: %w", err)
	}
	in, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		return nil, fmt.Errorf("sshclient: stdin pipe: %w", err)
	}
	out, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		return nil, fmt.Errorf("sshclient: stdout pipe: %w", err)
	}
	if err := sess.Shell(); err != nil {
		sess.Close()
		return nil, fmt.Errorf("sshclient: start shell: %w", err)
	}
	return &Shell{sess: sess, in: in, out: out}, nil
}

// WriteInput sends raw keyboard bytes to the remote PTY.
func (s *Shell) WriteInput(b []byte) (int, error) { return s.in.Write(b) }

// Output returns the reader streaming rendered PTY bytes.
func (s *Shell) Output() io.Reader { return s.out }

// Resize informs the remote PTY of a new window size.
func (s *Shell) Resize(cols, rows int) error {
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	return s.sess.WindowChange(rows, cols)
}

// Wait blocks until the remote shell exits.
func (s *Shell) Wait() error { return s.sess.Wait() }

// Close tears down the session.
func (s *Shell) Close() error { return s.sess.Close() }
