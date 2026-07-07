package server

import (
	"time"

	"emperror.dev/errors"

	"github.com/pterodactyl/wings/environment"
	"github.com/pterodactyl/wings/remote"
)

// Install executes the installation stack for a server process. Bubbles any
// errors up to the calling function which should handle contacting the panel to
// notify it of the server state.
//
// The actual installation mechanism is platform-specific: on Linux/macOS it
// runs the egg's install script inside a throwaway Docker container
// (install_notwindows.go); on Windows it runs the script natively
// (install_windows.go). Both expose the same InstallationProcess surface
// (NewInstallationProcess + Run) used here.
func (s *Server) Install() error {
	return s.install(false)
}

func (s *Server) install(reinstall bool) error {
	var err error
	if !s.Config().SkipEggScripts {
		// Send the start event so the Panel can automatically update. We don't
		// send this unless the process is actually going to run, otherwise all
		// sorts of weird rapid UI behavior happens since there isn't an actual
		// install process being executed.
		s.Events().Publish(InstallStartedEvent, "")

		err = s.internalInstall()
	} else {
		s.Log().Info("server configured to skip running installation scripts for this egg, not executing process")
	}

	s.Log().WithField("was_successful", err == nil).Debug("notifying panel of server install state")
	if serr := s.SyncInstallState(err == nil, reinstall); serr != nil {
		l := s.Log().WithField("was_successful", err == nil)

		// If the request was successful but there was an error with this request,
		// attach the error to this log entry. Otherwise, ignore it in this log
		// since whatever is calling this function should handle the error and
		// will end up logging the same one.
		if err == nil {
			l.WithField("error", err)
		}

		l.Warn("failed to notify panel of server install state")
	}

	// Ensure that the server is marked as offline at this point, otherwise you
	// end up with a blank value which is a bit confusing.
	s.Environment.SetState(environment.ProcessOfflineState)

	// Push an event to the websocket, so we can auto-refresh the information in
	// the panel once the installation is completed.
	s.Events().Publish(InstallCompletedEvent, "")

	return errors.WithStackIf(err)
}

// Reinstall reinstalls a server's software by utilizing the installation script
// for the server egg. This does not touch any existing files for the server,
// other than what the script modifies.
func (s *Server) Reinstall() error {
	if s.Environment.State() != environment.ProcessOfflineState {
		s.Log().Debug("waiting for server instance to enter a stopped state")
		if err := s.Environment.WaitForStop(s.Context(), time.Second*10, true); err != nil {
			return errors.WrapIf(err, "install: failed to stop running environment")
		}
	}

	s.Log().Info("syncing server state with remote source before executing re-installation process")
	if err := s.Sync(); err != nil {
		return errors.WrapIf(err, "install: failed to sync server state with Panel")
	}

	return s.install(true)
}

// Internal installation function used to simplify reporting back to the Panel.
func (s *Server) internalInstall() error {
	// Refuse to install an egg whose platform does not match this node (e.g. a
	// Linux egg on a Windows node). The egg's Docker image is the platform marker.
	if err := assertEggPlatform(s.Config().Container.Image); err != nil {
		return errors.WithStack(err)
	}

	script, err := s.client.GetInstallationScript(s.Context(), s.ID())
	if err != nil {
		return err
	}
	p, err := NewInstallationProcess(s, &script)
	if err != nil {
		return err
	}

	s.Log().Info("beginning installation process for server")
	if err := p.Run(); err != nil {
		return err
	}

	s.Log().Info("completed installation process for server")
	return nil
}

// IsInstalling returns if the server is actively running the installation
// process by checking the status of the installer lock.
func (s *Server) IsInstalling() bool {
	return s.installing.Load()
}

func (s *Server) SetInstalling(state bool) {
	s.installing.Store(state)
	if state {
		s.Sftp().CancelAll()
	}
}

func (s *Server) IsTransferring() bool {
	return s.transferring.Load()
}

func (s *Server) SetTransferring(state bool) {
	s.transferring.Store(state)
	if state {
		s.Sftp().CancelAll()
	}
}

func (s *Server) IsRestoring() bool {
	return s.restoring.Load()
}

func (s *Server) SetRestoring(state bool) {
	s.restoring.Store(state)
	if state {
		s.Sftp().CancelAll()
	}
}

func (s *Server) IsInProtectedState() bool {
	return s.IsInstalling() || s.IsTransferring() || s.IsRestoring()
}

// SyncInstallState makes an HTTP request to the Panel instance notifying it that
// the server has completed the installation process, and what the state of the
// server is.
func (s *Server) SyncInstallState(successful, reinstall bool) error {
	return s.client.SetInstallationStatus(s.Context(), s.ID(), remote.InstallStatusRequest{
		Successful: successful,
		Reinstall:  reinstall,
	})
}
