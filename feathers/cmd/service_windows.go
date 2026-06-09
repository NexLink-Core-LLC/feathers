//go:build windows

package cmd

import (
	"fmt"
	log2 "log"
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/pterodactyl/wings/config"
)

// serviceName is the Windows Service identifier used by the Service Control
// Manager. displayName/description are shown in services.msc.
const (
	serviceName        = "Wings"
	serviceDisplayName = "Pterodactyl Wings"
	serviceDescription = "Pterodactyl Wings daemon — runs game servers natively on Windows."
)

// runUnderServiceManager detects whether the process was launched by the
// Windows Service Control Manager. If so it runs the daemon under the service
// dispatcher and returns true; otherwise it returns false and the caller
// executes the command normally (interactive / CLI use).
func runUnderServiceManager() bool {
	isService, err := svc.IsWindowsService()
	if err != nil {
		log2.Printf("failed to determine if running as a windows service: %s", err)
		return false
	}
	if !isService {
		return false
	}
	if err := svc.Run(serviceName, &serviceHandler{}); err != nil {
		log2.Printf("windows service dispatcher failed: %s", err)
	}
	return true
}

// serviceHandler implements svc.Handler. It starts the daemon (the root command)
// in a goroutine and reports status to the SCM, stopping the process when a
// Stop/Shutdown control request is received.
type serviceHandler struct{}

func (h *serviceHandler) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown

	changes <- svc.Status{State: svc.StartPending}

	// Run the daemon. The configured service arguments (e.g. --config) are part
	// of os.Args, so cobra parses them normally and dispatches to the daemon.
	go func() {
		if err := rootCommand.Execute(); err != nil {
			log2.Printf("failed to execute daemon under service manager: %s", err)
			os.Exit(1)
		}
	}()

	changes <- svc.Status{State: svc.Running, Accepts: accepted}

	for c := range r {
		switch c.Cmd {
		case svc.Interrogate:
			changes <- c.CurrentStatus
		case svc.Stop, svc.Shutdown:
			changes <- svc.Status{State: svc.StopPending}
			// Wings does not perform graceful connection draining (it relies on
			// being terminated), so exiting the process is the correct stop
			// behavior and matches the Linux/systemd lifecycle.
			return false, 0
		default:
			log2.Printf("unexpected service control request #%d", c.Cmd)
		}
	}
	return false, 0
}

// newServiceCommand builds the `wings service ...` management subcommand
// (Windows only) for installing and controlling the daemon as a service.
func newServiceCommand() *cobra.Command {
	var configFlag string

	cmd := &cobra.Command{
		Use:   "service",
		Short: "Manage the Wings Windows Service (install/uninstall/start/stop/status).",
	}
	cmd.PersistentFlags().StringVar(&configFlag, "config", config.DefaultLocation, "config path embedded into the service definition")

	cmd.AddCommand(&cobra.Command{
		Use:   "install",
		Short: "Register Wings as an auto-starting Windows Service.",
		RunE: func(_ *cobra.Command, _ []string) error {
			return installService(configFlag)
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "uninstall",
		Short: "Remove the Wings Windows Service.",
		RunE:  func(_ *cobra.Command, _ []string) error { return uninstallService() },
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "start",
		Short: "Start the Wings Windows Service.",
		RunE:  func(_ *cobra.Command, _ []string) error { return controlService(svc.Continue, "start") },
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "stop",
		Short: "Stop the Wings Windows Service.",
		RunE:  func(_ *cobra.Command, _ []string) error { return controlService(svc.Stop, "stop") },
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show the Wings Windows Service status.",
		RunE:  func(_ *cobra.Command, _ []string) error { return statusService() },
	})
	return cmd
}

func init() {
	rootCommand.AddCommand(newServiceCommand())
}

func installService(configPath string) error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("could not determine executable path: %w", err)
	}

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("could not connect to service manager (run as Administrator): %w", err)
	}
	defer m.Disconnect()

	if s, err := m.OpenService(serviceName); err == nil {
		s.Close()
		return fmt.Errorf("service %q is already installed", serviceName)
	}

	s, err := m.CreateService(serviceName, exePath, mgr.Config{
		DisplayName:  serviceDisplayName,
		Description:  serviceDescription,
		StartType:    mgr.StartAutomatic,
		ErrorControl: mgr.ErrorNormal,
	}, "--config", configPath)
	if err != nil {
		return fmt.Errorf("could not create service: %w", err)
	}
	defer s.Close()

	fmt.Printf("Service %q installed.\n  Executable: %s\n  Config:     %s\nStart it with: wings service start\n", serviceName, exePath, configPath)
	return nil
}

func uninstallService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("could not connect to service manager (run as Administrator): %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("service %q is not installed", serviceName)
	}
	defer s.Close()

	if err := s.Delete(); err != nil {
		return fmt.Errorf("could not delete service: %w", err)
	}
	fmt.Printf("Service %q removed.\n", serviceName)
	return nil
}

func controlService(cmd svc.Cmd, action string) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("could not connect to service manager (run as Administrator): %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("service %q is not installed", serviceName)
	}
	defer s.Close()

	if action == "start" {
		if err := s.Start(); err != nil {
			return fmt.Errorf("could not start service: %w", err)
		}
		fmt.Printf("Service %q started.\n", serviceName)
		return nil
	}

	if _, err := s.Control(cmd); err != nil {
		return fmt.Errorf("could not send %s control to service: %w", action, err)
	}
	fmt.Printf("Service %q %sped.\n", serviceName, action)
	return nil
}

func statusService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("could not connect to service manager: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("service %q is not installed", serviceName)
	}
	defer s.Close()

	st, err := s.Query()
	if err != nil {
		return fmt.Errorf("could not query service: %w", err)
	}

	states := map[svc.State]string{
		svc.Stopped:         "stopped",
		svc.StartPending:    "start pending",
		svc.StopPending:     "stop pending",
		svc.Running:         "running",
		svc.ContinuePending: "continue pending",
		svc.PausePending:    "pause pending",
		svc.Paused:          "paused",
	}
	label, ok := states[st.State]
	if !ok {
		label = fmt.Sprintf("unknown (%d)", st.State)
	}
	fmt.Printf("Service %q: %s\n", serviceName, label)
	return nil
}
