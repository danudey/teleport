/*
Copyright 2015 Gravitational, Inc.

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

package srv

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/lib/bpf"
	"github.com/gravitational/teleport/lib/events"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/utils"

	"github.com/gravitational/trace"
	log "github.com/sirupsen/logrus"
)

const (
	defaultPath          = "/bin:/usr/bin:/usr/local/bin:/sbin"
	defaultEnvPath       = "PATH=" + defaultPath
	defaultTerm          = "xterm"
	defaultLoginDefsPath = "/etc/login.defs"
)

// execCommand contains the payload to "teleport exec" will will be used to
// construct and execute a shell.
type execCommand struct {
	// CommandType is the type of command to re-execute. It's either an exec
	// command (for a shell or exec command) or a forward command for port
	// forwarding.
	Type string `json:"type"`

	// Command is the command to execute. If a interactive session is being
	// requested, will be empty.
	Command string `json:"command"`

	// DestinationAddress is the target address to dial to.
	DestinationAddress string `json:"dst_addr"`

	// Username is the username associated with the Teleport identity.
	Username string `json:"username"`

	// Login is the local *nix account.
	Login string `json:"login"`

	// Roles is the list of Teleport roles assigned to the Teleport identity.
	Roles []string `json:"roles"`

	// ClusterName is the name of the Teleport cluster.
	ClusterName string `json:"cluster_name"`

	// Terminal indicates if a TTY has been allocated for the session. This is
	// typically set if either an shell was requested or a TTY was explicitly
	// allocated for a exec request.
	Terminal bool `json:"term"`

	// RequestType is the type of request: either "exec" or "shell". This will
	// be used to control where to connect std{out,err} based on the request
	// type: "exec" or "shell".
	RequestType string `json:"request_type"`

	// PAM indicates if PAM support was requested by the node.
	PAM bool `json:"pam"`

	// ServiceName is the name of the PAM service requested if PAM is enabled.
	ServiceName string `json:"service_name"`

	// Environment is a list of environment variables to add to the defaults.
	Environment []string `json:"environment"`

	// PermitUserEnvironment is set to allow reading in ~/.tsh/environment
	// upon login.
	PermitUserEnvironment bool `json:"permit_user_environment"`

	// IsTestStub is used by tests to mock the shell.
	IsTestStub bool `json:"is_test_stub"`
}

// ExecResult is used internally to send the result of a command execution from
// a goroutine to SSH request handler and back to the calling client
type ExecResult struct {
	// Command is the command that was executed.
	Command string

	// Code is return code that execution of the command resulted in.
	Code int
}

// Exec executes an "exec" request.
type Exec interface {
	// GetCommand returns the command to be executed.
	GetCommand() string

	// SetCommand sets the command to be executed.
	SetCommand(string)

	// Start will start the execution of the command.
	Start(channel ssh.Channel) (*ExecResult, error)

	// Wait will block while the command executes.
	Wait() *ExecResult

	// Continue will resume execution of the process after it completes its
	// pre-processing routine (placed in a cgroup).
	Continue()

	// PID returns the PID of the Teleport process that was re-execed.
	PID() int
}

// NewExecRequest creates a new local or remote Exec.
func NewExecRequest(ctx *ServerContext, command string) (Exec, error) {
	// It doesn't matter what mode the cluster is in, if this is a Teleport node
	// return a local *localExec.
	if ctx.srv.Component() == teleport.ComponentNode {
		return &localExec{
			Ctx:     ctx,
			Command: command,
		}, nil
	}

	// When in recording mode, return an *remoteExec which will execute the
	// command on a remote host. This is used by in-memory forwarding nodes.
	if ctx.ClusterConfig.GetSessionRecording() == services.RecordAtProxy {
		return &remoteExec{
			ctx:     ctx,
			command: command,
			session: ctx.RemoteSession,
		}, nil
	}

	// Otherwise return a *localExec which will execute locally on the server.
	// used by the regular Teleport nodes.
	return &localExec{
		Ctx:     ctx,
		Command: command,
	}, nil
}

// localExec prepares the response to a 'exec' SSH request, i.e. executing
// a command after making an SSH connection and delivering the result back.
type localExec struct {
	// Command is the command that will be executed.
	Command string

	// Cmd holds an *exec.Cmd which will be used for local execution.
	Cmd *exec.Cmd

	// Ctx holds the *ServerContext.
	Ctx *ServerContext

	// sessionContext holds the BPF session context used to lookup and interact
	// with BPF sessions.
	sessionContext *bpf.SessionContext
}

// GetCommand returns the command string.
func (e *localExec) GetCommand() string {
	return e.Command
}

// SetCommand gets the command string.
func (e *localExec) SetCommand(command string) {
	e.Command = command
}

// Start launches the given command returns (nil, nil) if successful.
// ExecResult is only used to communicate an error while launching.
func (e *localExec) Start(channel ssh.Channel) (*ExecResult, error) {
	// Parse the command to see if it is scp.
	err := e.transformSecureCopy()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Create the command that will actually execute.
	e.Cmd, err = ConfigureCommand(e.Ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Connect stdout and stderr to the channel so the user can interact with
	// the command.
	e.Cmd.Stderr = channel.Stderr()
	e.Cmd.Stdout = channel

	// Copy from the channel (client input) into stdin of the process.
	inputWriter, err := e.Cmd.StdinPipe()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	go func() {
		io.Copy(inputWriter, channel)
		inputWriter.Close()
	}()

	// Start the command.
	err = e.Cmd.Start()
	if err != nil {
		e.Ctx.Warningf("Local command %v failed to start: %v", e.GetCommand(), err)

		// Emit the result of execution to the audit log
		emitExecAuditEvent(e.Ctx, e.GetCommand(), err)

		return &ExecResult{
			Command: e.GetCommand(),
			Code:    exitCode(err),
		}, trace.ConvertSystemError(err)
	}

	e.Ctx.Infof("Started local command execution: %q", e.Command)

	return nil, nil
}

// Wait will block while the command executes.
func (e *localExec) Wait() *ExecResult {
	if e.Cmd.Process == nil {
		e.Ctx.Errorf("no process")
	}

	// Block until the command is finished executing.
	err := e.Cmd.Wait()
	if err != nil {
		e.Ctx.Debugf("Local command failed: %v.", err)
	} else {
		e.Ctx.Debugf("Local command successfully executed.")
	}

	// Emit the result of execution to the Audit Log.
	emitExecAuditEvent(e.Ctx, e.GetCommand(), err)

	execResult := &ExecResult{
		Command: e.GetCommand(),
		Code:    exitCode(err),
	}

	return execResult
}

// Continue will resume execution of the process after it completes its
// pre-processing routine (placed in a cgroup).
func (e *localExec) Continue() {
	e.Ctx.contw.Close()

	// Set to nil so the close in the context doesn't attempt to re-close.
	e.Ctx.contw = nil
}

// PID returns the PID of the Teleport process that was re-execed.
func (e *localExec) PID() int {
	return e.Cmd.Process.Pid
}

func (e *localExec) String() string {
	return fmt.Sprintf("Exec(Command=%v)", e.Command)
}

func (e *localExec) transformSecureCopy() error {
	// split up command by space to grab the first word. if we don't have anything
	// it's an interactive shell the user requested and not scp, return
	args := strings.Split(e.GetCommand(), " ")
	if len(args) == 0 {
		return nil
	}

	// see the user is not requesting scp, return
	_, f := filepath.Split(args[0])
	if f != teleport.SCP {
		return nil
	}

	// for scp requests update the command to execute to launch teleport with
	// scp parameters just like openssh does.
	teleportBin, err := os.Executable()
	if err != nil {
		return trace.Wrap(err)
	}
	e.Command = fmt.Sprintf("%s scp --remote-addr=%s --local-addr=%s %v",
		teleportBin,
		e.Ctx.Conn.RemoteAddr().String(),
		e.Ctx.Conn.LocalAddr().String(),
		strings.Join(args[1:], " "))

	return nil
}

// waitForContinue will wait 10 seconds for the continue signal, if not
// received, it will stop waiting and exit.
func waitForContinue(contfd *os.File) error {
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		// Reading from the continue file descriptor will block until it's closed. It
		// won't be closed until the parent has placed it in a cgroup.
		var r bytes.Buffer
		r.ReadFrom(contfd)

		// Continue signal has been processed, signal to continue execution.
		cancel()
	}()

	// Wait for 10 seconds and then timeout if no continue signal has been sent.
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()

	select {
	case <-timeout.C:
		return trace.BadParameter("timed out waiting for continue signal")
	case <-ctx.Done():
	}
	return nil
}

// remoteExec is used to run an "exec" SSH request and return the result.
type remoteExec struct {
	command string
	session *ssh.Session
	ctx     *ServerContext
}

// GetCommand returns the command string.
func (e *remoteExec) GetCommand() string {
	return e.command
}

// SetCommand gets the command string.
func (e *remoteExec) SetCommand(command string) {
	e.command = command
}

// Start launches the given command returns (nil, nil) if successful.
// ExecResult is only used to communicate an error while launching.
func (r *remoteExec) Start(ch ssh.Channel) (*ExecResult, error) {
	// hook up stdout/err the channel so the user can interact with the command
	r.session.Stdout = ch
	r.session.Stderr = ch.Stderr()
	inputWriter, err := r.session.StdinPipe()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	go func() {
		// copy from the channel (client) into stdin of the process
		io.Copy(inputWriter, ch)
		inputWriter.Close()
	}()

	err = r.session.Start(r.command)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return nil, nil
}

// Wait will block while the command executes.
func (r *remoteExec) Wait() *ExecResult {
	// Block until the command is finished executing.
	err := r.session.Wait()
	if err != nil {
		r.ctx.Debugf("Remote command failed: %v.", err)
	} else {
		r.ctx.Debugf("Remote command successfully executed.")
	}

	// Emit the result of execution to the Audit Log.
	emitExecAuditEvent(r.ctx, r.command, err)

	return &ExecResult{
		Command: r.GetCommand(),
		Code:    exitCode(err),
	}
}

// Continue does nothing for remote command execution.
func (r *remoteExec) Continue() {
	return
}

// PID returns an invalid PID for remotExec.
func (r *remoteExec) PID() int {
	return 0
}

func emitExecAuditEvent(ctx *ServerContext, cmd string, execErr error) {
	// Report the result of this exec event to the audit logger.
	auditLog := ctx.srv.GetAuditLog()
	if auditLog == nil {
		log.Warnf("No audit log")
		return
	}

	var event events.Event

	// Create common fields for event.
	fields := events.EventFields{
		events.EventUser:      ctx.Identity.TeleportUser,
		events.EventLogin:     ctx.Identity.Login,
		events.LocalAddr:      ctx.Conn.LocalAddr().String(),
		events.RemoteAddr:     ctx.Conn.RemoteAddr().String(),
		events.EventNamespace: ctx.srv.GetNamespace(),
		// Due to scp being inherently vulnerable to command injection, always
		// make sure the full command and exit code is recorded for accountability.
		// For more details, see the following.
		//
		// https://bugs.debian.org/cgi-bin/bugreport.cgi?bug=327019
		// https://bugzilla.mindrot.org/show_bug.cgi?id=1998
		events.ExecEventCode:    strconv.Itoa(exitCode(execErr)),
		events.ExecEventCommand: cmd,
	}
	if execErr != nil {
		fields[events.ExecEventError] = execErr.Error()
	}

	// Parse the exec command to find out if it was SCP or not.
	path, action, isSCP, err := parseSecureCopy(cmd)
	if err != nil {
		log.Warnf("Unable to emit audit event: %v.", err)
		return
	}

	// Update appropriate fields based off if the request was SCP or not.
	if isSCP {
		fields[events.SCPPath] = path
		fields[events.SCPAction] = action
		switch action {
		case events.SCPActionUpload:
			if execErr != nil {
				event = events.SCPUploadFailure
			} else {
				event = events.SCPUpload
			}
		case events.SCPActionDownload:
			if execErr != nil {
				event = events.SCPDownloadFailure
			} else {
				event = events.SCPDownload
			}
		}
	} else {
		if execErr != nil {
			event = events.ExecFailure
		} else {
			event = events.Exec
		}
	}

	// Emit the event.
	auditLog.EmitAuditEvent(event, fields)
}

// getDefaultEnvPath returns the default value of PATH environment variable for
// new logins (prior to shell) based on login.defs. Returns a strings which
// looks like "PATH=/usr/bin:/bin"
func getDefaultEnvPath(uid string, loginDefsPath string) string {
	envPath := defaultEnvPath
	envSuPath := defaultEnvPath

	// open file, if it doesn't exist return a default path and move on
	f, err := os.Open(loginDefsPath)
	if err != nil {
		log.Infof("Unable to open %q: %v: returning default path: %q", loginDefsPath, err, defaultEnvPath)
		return defaultEnvPath
	}
	defer f.Close()

	// read path to login.defs file /etc/login.defs line by line:
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// skip comments and empty lines:
		if line == "" || line[0] == '#' {
			continue
		}

		// look for a line that starts with ENV_SUPATH or ENV_PATH
		fields := strings.Fields(line)
		if len(fields) > 1 {
			if fields[0] == "ENV_PATH" {
				envPath = fields[1]
			}
			if fields[0] == "ENV_SUPATH" {
				envSuPath = fields[1]
			}
		}
	}

	// if any error occurs while reading the file, return the default value
	err = scanner.Err()
	if err != nil {
		log.Warnf("Unable to read %q: %v: returning default path: %q", loginDefsPath, err, defaultEnvPath)
		return defaultEnvPath
	}

	// if requesting path for uid 0 and no ENV_SUPATH is given, fallback to
	// ENV_PATH first, then the default path.
	if uid == "0" {
		if envSuPath == defaultEnvPath {
			return envPath
		}
		return envSuPath
	}
	return envPath
}

// parseSecureCopy will parse a command and return if it's secure copy or not.
func parseSecureCopy(path string) (string, string, bool, error) {
	parts := strings.Fields(path)
	if len(parts) == 0 {
		return "", "", false, trace.BadParameter("no executable found")
	}

	// Look for the -t flag, it indicates that an upload occurred. The other
	// flags do no matter for now.
	action := events.SCPActionDownload
	if utils.SliceContainsStr(parts, "-t") {
		action = events.SCPActionUpload
	}

	// Exract the name of the Teleport executable on disk.
	teleportPath, err := os.Executable()
	if err != nil {
		return "", "", false, trace.Wrap(err)
	}
	_, teleportBinary := filepath.Split(teleportPath)

	// Extract the name of the executable that was run. The command was secure
	// copy if the executable was "scp" or "teleport".
	_, executable := filepath.Split(parts[0])
	switch executable {
	case teleport.SCP, teleportBinary:
		return parts[len(parts)-1], action, true, nil
	default:
		return "", "", false, nil
	}
}

// exitCode extracts and returns the exit code from the error.
func exitCode(err error) int {
	// If no error occurred, return 0 (success).
	if err == nil {
		return teleport.RemoteCommandSuccess
	}

	switch v := err.(type) {
	// Local execution.
	case *exec.ExitError:
		waitStatus, ok := v.Sys().(syscall.WaitStatus)
		if !ok {
			return teleport.RemoteCommandFailure
		}
		return waitStatus.ExitStatus()
	// Remote execution.
	case *ssh.ExitError:
		return v.ExitStatus()
	// An error occurred, but the type is unknown, return a generic 255 code.
	default:
		log.Debugf("Unknown error returned when executing command: %T: %v.", err, err)
		return teleport.RemoteCommandFailure
	}
}
