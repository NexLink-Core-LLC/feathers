//go:build windows

package config

// DefaultLocation is the default path to the Wings configuration file on
// Windows. C:\Pterodactyl mirrors the conventional Linux /etc + /var layout in
// a single, predictable location for the daemon, its config, and server data.
const DefaultLocation = `C:\Pterodactyl\config.yml`
