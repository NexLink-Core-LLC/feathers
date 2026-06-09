//go:build windows

package server

import (
	"bytes"
	"context"
	"html/template"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"emperror.dev/errors"
	"golang.org/x/sys/windows"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/remote"
	"github.com/pterodactyl/wings/system"
)

// InstallationProcess runs an egg's installation script natively on Windows.
//
// On Linux the script runs inside a throwaway Docker container; there is no
// container here, so the script is executed directly by PowerShell in the
// server's data directory with the egg's environment variables. Windows-targeted
// eggs must therefore ship PowerShell-compatible install scripts (the egg's
// Docker image and Linux entrypoint are ignored — they only appear in the log
// header). This is the Windows half of the Phase 4 installer port; it lets
// automated provisioners (e.g. PteroBill/WHMCS) create servers without any
// manual "skip install script" intervention.
type InstallationProcess struct {
	Server *Server
	Script *remote.InstallationScript
}

// NewInstallationProcess returns a native Windows installation process. Unlike
// the Docker backend there is no client to construct, so this never fails.
func NewInstallationProcess(s *Server, script *remote.InstallationScript) (*InstallationProcess, error) {
	return &InstallationProcess{Server: s, Script: script}, nil
}

// tempDir returns the per-server scratch directory for the install script.
func (ip *InstallationProcess) tempDir() string {
	return filepath.Join(config.Get().System.TmpDirectory, ip.Server.ID())
}

// GetLogPath returns the path of the persisted installation log.
func (ip *InstallationProcess) GetLogPath() string {
	return filepath.Join(config.Get().System.LogDirectory, "/install", ip.Server.ID()+".log")
}

// writeScriptToDisk writes the egg's install script to a temporary .ps1 file,
// normalising line endings to CRLF so PowerShell and native tools read it
// cleanly. Returns the script path.
func (ip *InstallationProcess) writeScriptToDisk() (string, error) {
	if err := os.MkdirAll(ip.tempDir(), 0o700); err != nil {
		return "", errors.WithMessage(err, "could not create temporary directory for install process")
	}
	p := filepath.Join(ip.tempDir(), "install.ps1")
	normalised := strings.ReplaceAll(strings.ReplaceAll(ip.Script.Script, "\r\n", "\n"), "\n", "\r\n")
	if err := os.WriteFile(p, []byte(normalised), 0o644); err != nil {
		return "", errors.WithMessage(err, "failed to write server installation script to disk")
	}
	return p, nil
}

// Run executes the installation script and persists its output. It mirrors the
// Docker backend's success semantics: the install is considered successful as
// long as the script process ran to completion. A non-zero script exit code is
// logged but does not fail the install (matching upstream, where the container
// exit code is not treated as an install failure), so a server still becomes
// usable and can be debugged from its install log.
func (ip *InstallationProcess) Run() error {
	ip.Server.Log().Debug("acquiring installation process lock")
	if !ip.Server.installing.SwapIf(true) {
		return errors.New("install: cannot obtain installation lock")
	}
	defer func() {
		ip.Server.Log().Debug("releasing installation process lock")
		ip.Server.installing.Store(false)
	}()

	if err := ip.Server.EnsureDataDirectoryExists(); err != nil {
		return err
	}

	scriptPath, err := ip.writeScriptToDisk()
	if err != nil {
		return err
	}
	defer func() {
		if err := os.RemoveAll(ip.tempDir()); err != nil && !os.IsNotExist(err) {
			ip.Server.Log().WithField("error", err).Warn("failed to remove temporary data directory after install process")
		}
	}()

	// Cancel the install if the server is deleted mid-process.
	ctx, cancel := context.WithCancel(ip.Server.Context())
	defer cancel()

	ip.Server.Events().Publish(DaemonMessageEvent, "Starting installation process, this could take a few minutes...")
	ip.Server.Log().WithField("install_script", scriptPath).Info("running installation script for server natively")

	cmd := exec.CommandContext(ctx, "powershell.exe",
		"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", scriptPath)
	cmd.Dir = ip.Server.Filesystem().Path()
	cmd.Env = mergeInstallEnv(ip.Server.GetEnvironmentVariables())
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}

	// Merge stdout+stderr into a single pipe; tee it so we can both stream to the
	// Panel install console and capture it for the on-disk log.
	pr, pw, err := os.Pipe()
	if err != nil {
		return errors.Wrap(err, "install: failed to create output pipe")
	}
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		_ = pr.Close()
		return errors.Wrap(err, "install: failed to start installation script")
	}
	// The child holds its own copy of the write end now.
	_ = pw.Close()

	var buf bytes.Buffer
	streamed := make(chan struct{})
	go func() {
		defer close(streamed)
		if serr := system.ScanReader(io.TeeReader(pr, &buf), ip.Server.Sink(system.InstallSink).Push); serr != nil && !errors.Is(serr, context.Canceled) {
			ip.Server.Log().WithField("error", serr).Warn("error processing install output lines")
		}
	}()

	werr := cmd.Wait()
	<-streamed
	_ = pr.Close()

	if werr != nil {
		var ee *exec.ExitError
		if errors.As(werr, &ee) {
			// Non-zero exit: log it but treat the install as completed, matching the
			// Docker backend which does not fail on script exit code.
			ip.Server.Log().WithField("exit_code", ee.ExitCode()).Warn("installation script exited with a non-zero status")
		} else {
			return errors.Wrap(werr, "install: installation script failed to run")
		}
	}

	ip.Server.Events().Publish(DaemonMessageEvent, "Installation process completed.")

	if err := ip.writeLog(buf.Bytes()); err != nil {
		ip.Server.Log().WithField("error", err).Warn("failed to write installation log to disk")
	}
	return nil
}

// mergeInstallEnv overlays the egg's environment variables onto the daemon's
// base Windows environment so install scripts (SteamCMD, etc.) have SystemRoot,
// SystemDrive, PATH, ProgramData and friends. Without them, native tooling fails
// to expand %VAR% references (e.g. SteamCMD creating a literal "%SystemDrive%"
// folder and not installing). Keys are merged case-insensitively.
func mergeInstallEnv(eggVars []string) []string {
	byKey := make(map[string]string)
	var order []string
	put := func(kv string) {
		i := strings.IndexByte(kv, '=')
		if i < 0 {
			return
		}
		key := strings.ToUpper(kv[:i])
		if _, ok := byKey[key]; !ok {
			order = append(order, key)
		}
		byKey[key] = kv
	}
	for _, kv := range os.Environ() {
		put(kv)
	}
	for _, kv := range eggVars {
		put(kv)
	}
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, byKey[k])
	}
	return out
}

// writeLog persists the captured install output to the server's install log,
// with a header mirroring the Docker backend's format for parity.
func (ip *InstallationProcess) writeLog(output []byte) error {
	logPath := ip.GetLogPath()
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	tmpl, err := template.New("header").Parse(`Pterodactyl Server Installation Log (Windows / winproc)

|
| Details
| ------------------------------
  Server UUID:          {{.Server.ID}}
  Container Image:      {{.Script.ContainerImage}} (ignored on Windows)
  Container Entrypoint: {{.Script.Entrypoint}} (ignored on Windows; run via PowerShell)

|
| Environment Variables
| ------------------------------
{{ range $key, $value := .Server.GetEnvironmentVariables }}  {{ $value }}
{{ end }}

|
| Script Output
| ------------------------------
`)
	if err != nil {
		return err
	}
	if err := tmpl.Execute(f, ip); err != nil {
		return err
	}
	if _, err := f.Write(output); err != nil {
		return err
	}
	return nil
}
