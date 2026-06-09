//go:build !windows

package config

// DefaultLocation is the default path to the Wings configuration file on
// Unix-like systems.
const DefaultLocation = "/etc/pterodactyl/config.yml"
