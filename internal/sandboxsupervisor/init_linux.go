//go:build linux

package sandboxsupervisor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const maximumInitOutputBytes = 4096

type initRequest struct {
	Action string `json:"action"`
	Source string `json:"source,omitempty"`
	Stdin  string `json:"stdin,omitempty"`
}

type initResponse struct {
	Phase     string `json:"phase"`
	PID       int    `json:"pid,omitempty"`
	ExitCode  int    `json:"exit_code"`
	Stdout    string `json:"stdout,omitempty"`
	Stderr    string `json:"stderr,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

type initMessage struct {
	request initRequest
	err     error
}

// RunInit is the trusted Profile Bundle entry point. Its pipes are inherited
// from the Supervisor; neither the Worker socket nor a Workload can reach them.
func RunInit() error {
	if os.Getpid() != 1 || os.Geteuid() != 0 || os.Getegid() != 0 {
		return errors.New("Sandbox Init requires namespace PID 1 and internal UID/GID zero")
	}
	ready := os.NewFile(3, "init-ready")
	control := os.NewFile(4, "init-requests")
	responses := os.NewFile(6, "init-responses")
	defer ready.Close()
	defer control.Close()
	defer responses.Close()
	for _, file := range []*os.File{ready, control, responses} {
		info, err := file.Stat()
		if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
			return errors.New("Sandbox Init requires private inherited pipes")
		}
		syscall.CloseOnExec(int(file.Fd()))
	}
	children := make(chan os.Signal, 1)
	signal.Notify(children, syscall.SIGCHLD)
	defer signal.Stop(children)
	stopped := make(chan struct{})
	defer close(stopped)
	requests := make(chan initMessage, 1)
	go readInitRequests(control, requests, stopped)
	if _, err := ready.Write([]byte{'R'}); err != nil {
		return err
	}
	if err := ready.Close(); err != nil {
		return err
	}
	for {
		message := <-requests
		if message.err != nil {
			return message.err
		}
		request := message.request
		if request.Action != "start" || request.Source == "" || strings.ContainsRune(request.Source, 0) || len(request.Source) > 32<<10 || len(request.Stdin) > 8<<10 {
			return errors.New("invalid Sandbox Init start request")
		}
		if err := executeInitWorkload(request, requests, responses, children); err != nil {
			return err
		}
	}
}

func readInitRequests(control io.Reader, requests chan<- initMessage, stopped <-chan struct{}) {
	for {
		payload, err := readFrame(control)
		var request initRequest
		if err == nil {
			err = decodeStrictJSON(payload, &request, "action", "source", "stdin")
		}
		select {
		case requests <- initMessage{request: request, err: err}:
		case <-stopped:
			return
		}
		if err != nil {
			return
		}
	}
}

func writeInitResponse(responses io.Writer, response initResponse) error {
	payload, err := json.Marshal(response)
	if err != nil {
		return err
	}
	return writeFrame(responses, payload)
}

func requireInitAction(requests <-chan initMessage, action string) error {
	message := <-requests
	if message.err != nil {
		return message.err
	}
	if message.request.Action != action || message.request.Source != "" || message.request.Stdin != "" {
		return fmt.Errorf("Sandbox Init expected %s", action)
	}
	return nil
}

func executeInitWorkload(request initRequest, requests <-chan initMessage, responses io.Writer, children <-chan os.Signal) error {
	// Explicit files prevent os/exec from opening /dev/null or waiting on
	// copy goroutines whose pipe writers could be retained by descendants.
	var files []*os.File
	defer func() {
		for _, file := range files {
			_ = file.Close()
		}
	}()
	pipe := func() (*os.File, *os.File, error) {
		reader, writer, err := os.Pipe()
		if err == nil {
			files = append(files, reader, writer)
		}
		return reader, writer, err
	}
	stdinReader, stdinWriter, err := pipe()
	if err != nil {
		return err
	}
	stdoutReader, stdoutWriter, err := pipe()
	if err != nil {
		return err
	}
	stderrReader, stderrWriter, err := pipe()
	if err != nil {
		return err
	}
	gateReader, gateWriter, err := pipe()
	if err != nil {
		return err
	}
	process, err := os.StartProcess("/sandbox-init", []string{"sandbox-init", "--workload", request.Source}, &os.ProcAttr{
		Dir:   "/workspace/output",
		Env:   initWorkloadEnvironment(),
		Files: []*os.File{stdinReader, stdoutWriter, stderrWriter, gateReader},
		Sys: &syscall.SysProcAttr{
			Credential: &syscall.Credential{Uid: 1000, Gid: 1000, Groups: []uint32{}},
		},
	})
	if err != nil {
		return fmt.Errorf("start Sandbox Workload launcher: %w", err)
	}
	defer process.Release()
	_ = stdinReader.Close()
	_ = stdoutWriter.Close()
	_ = stderrWriter.Close()
	_ = gateReader.Close()
	stdout := captureInitOutput(stdoutReader)
	stderr := captureInitOutput(stderrReader)
	go func() {
		_, _ = io.WriteString(stdinWriter, request.Stdin)
		_ = stdinWriter.Close()
	}()
	if err := writeInitResponse(responses, initResponse{Phase: "started", PID: process.Pid}); err != nil {
		return err
	}
	// The Supervisor moves the blocked launcher into the Execution's child
	// cgroup before releasing this gate. No Python code runs before that.
	if err := requireInitAction(requests, "run"); err != nil {
		return err
	}
	if _, err := gateWriter.Write([]byte{'G'}); err != nil {
		return err
	}
	_ = gateWriter.Close()
	exitCode, err := waitInitChildren(process.Pid, requests, children)
	if err != nil {
		return err
	}
	_ = stdinWriter.Close()
	if err := writeInitResponse(responses, initResponse{Phase: "exited", ExitCode: exitCode}); err != nil {
		return err
	}
	// Descendants can retain stdout/stderr after the main child exits. First
	// let the Supervisor kill the entire child cgroup, then reap and drain.
	if err := requireInitAction(requests, "reap"); err != nil {
		return err
	}
	if _, err := waitInitChildren(0, requests, children); err != nil {
		return err
	}
	output, err := awaitInitOutput(stdout, requests)
	if err != nil {
		return err
	}
	errorOutput, err := awaitInitOutput(stderr, requests)
	if err != nil {
		return err
	}
	return writeInitResponse(responses, initResponse{
		Phase: "ready", ExitCode: exitCode, Stdout: output.text, Stderr: errorOutput.text,
		Truncated: output.truncated || errorOutput.truncated,
	})
}

// One waiter owns all children. Competing Process.Wait and Wait4(-1) calls
// could steal each other's exit status, so reap adopted children here too.
// A zero mainPID means the Supervisor has killed the Execution's cgroup and
// we must reach ECHILD before admitting the next Execution.
func waitInitChildren(mainPID int, requests <-chan initMessage, children <-chan os.Signal) (int, error) {
	var timeout <-chan time.Time
	if mainPID == 0 {
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		timeout = timer.C
	}
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		if err == syscall.EINTR {
			continue
		}
		if err == syscall.ECHILD && mainPID == 0 {
			return 0, nil
		}
		if err != nil {
			return 0, fmt.Errorf("reap Sandbox children: %w", err)
		}
		if pid == mainPID && pid != 0 {
			if status.Signaled() {
				return 128 + int(status.Signal()), nil
			}
			return status.ExitStatus(), nil
		}
		if pid != 0 {
			continue
		}
		select {
		case <-children:
		case message := <-requests:
			if message.err != nil {
				return 0, message.err
			}
			return 0, errors.New("unexpected Sandbox Init request while waiting for children")
		case <-timeout:
			return 0, errors.New("Sandbox descendants did not exit after cgroup termination")
		}
	}
}

type initOutput struct {
	text      string
	truncated bool
	err       error
}

func captureInitOutput(reader io.Reader) <-chan initOutput {
	result := make(chan initOutput, 1)
	go func() {
		retained := make([]byte, 0, maximumInitOutputBytes)
		var buffer [8192]byte
		output := initOutput{}
		for {
			n, err := reader.Read(buffer[:])
			remaining := maximumInitOutputBytes - len(retained)
			if n > remaining {
				output.truncated = true
			}
			retained = append(retained, buffer[:min(n, remaining)]...)
			if err != nil {
				if err != io.EOF {
					output.err = err
				}
				break
			}
		}
		output.text = strings.ToValidUTF8(string(retained), "\uFFFD")
		if len(output.text) > maximumInitOutputBytes {
			output.text = output.text[:maximumInitOutputBytes]
			for !utf8.ValidString(output.text) {
				output.text = output.text[:len(output.text)-1]
			}
			output.truncated = true
		}
		result <- output
	}()
	return result
}

func awaitInitOutput(output <-chan initOutput, requests <-chan initMessage) (initOutput, error) {
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case captured := <-output:
		return captured, captured.err
	case message := <-requests:
		if message.err != nil {
			return initOutput{}, message.err
		}
		return initOutput{}, errors.New("unexpected Sandbox Init request while draining output")
	case <-timer.C:
		return initOutput{}, errors.New("Sandbox output pipes remained open after descendant reaping")
	}
}

func initWorkloadEnvironment() []string {
	return []string{
		"HOME=/workspace/output", "TMPDIR=/tmp", "PATH=/opt/python/bin",
		"GOMAXPROCS=1", "GODEBUG=containermaxprocs=0,updatemaxprocs=0",
	}
}

// RunInitWorkload is the trusted, unprivileged launch gate. Only its inherited
// gate and stdio survive the first exec; the gate is closed before Python.
func RunInitWorkload(source string) error {
	if os.Getpid() == 1 || os.Geteuid() != 1000 || os.Getegid() != 1000 {
		return errors.New("Sandbox Workload requires internal UID/GID 1000")
	}
	gate := os.NewFile(3, "workload-gate")
	defer gate.Close()
	info, err := gate.Stat()
	if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		return errors.New("Sandbox Workload requires a private inherited launch gate")
	}
	syscall.CloseOnExec(3)
	var permission [1]byte
	if _, err := io.ReadFull(gate, permission[:]); err != nil || permission[0] != 'G' {
		return errors.New("Sandbox Workload was not released by the Supervisor")
	}
	if err := gate.Close(); err != nil {
		return err
	}
	return syscall.Exec("/opt/python/bin/python3", []string{"python3", "-I", "-B", "-c", source}, initWorkloadEnvironment())
}
