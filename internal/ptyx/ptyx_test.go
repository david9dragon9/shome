package ptyx

import (
	"bufio"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestOpenAndClose(t *testing.T) {
	p, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if p.Master == nil || p.Slave == nil {
		t.Fatal("Open returned an incomplete pair")
	}
	if err := p.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	// Idempotent: a session that failed partway will close twice.
	if err := p.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// The point of a pty: the program on the other end believes it is a terminal.
func TestProgramSeesATerminal(t *testing.T) {
	p, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	cmd := exec.Command("/bin/sh", "-c", "test -t 0 && echo IS-A-TTY || echo NOT-A-TTY")
	p.Attach(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p.CloseSlave()

	line, err := bufio.NewReader(p.Master).ReadString('\n')
	if err != nil && line == "" {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(line, "IS-A-TTY") {
		t.Errorf("the child did not see a terminal: %q", line)
	}
	cmd.Wait()
}

// Reads on the master must reach end-of-file when the child exits, or a
// session would hang open forever after the shell has gone. That is what
// CloseSlave is for.
func TestMasterSeesEOFWhenTheChildExits(t *testing.T) {
	p, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	cmd := exec.Command("/bin/sh", "-c", "echo done")
	p.Attach(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p.CloseSlave()

	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 256)
		for {
			if _, err := p.Master.Read(buf); err != nil {
				return
			}
		}
	}()
	cmd.Wait()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reading the master never ended after the child exited")
	}
}

// Typing has to reach the program, which is the other half of interactive.
func TestInputReachesTheProgram(t *testing.T) {
	p, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	cmd := exec.Command("/bin/cat")
	p.Attach(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p.CloseSlave()
	defer func() { cmd.Process.Kill(); cmd.Wait() }()

	if _, err := p.Master.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	p.Master.SetReadDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(p.Master)
	// A terminal echoes, so "hello" comes back before cat's own copy.
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(line, "hello") {
		t.Errorf("input did not reach the program: %q", line)
	}
}

func TestResize(t *testing.T) {
	p, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	if err := p.Resize(40, 100, 0, 0); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	// The size must be what the program on the other end reads back.
	cmd := exec.Command("/bin/sh", "-c", "stty size")
	p.Attach(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p.CloseSlave()
	line, _ := bufio.NewReader(p.Master).ReadString('\n')
	cmd.Wait()
	if !strings.Contains(line, "40 100") {
		t.Errorf("stty size reported %q, want \"40 100\"", strings.TrimSpace(line))
	}
}
