//go:build windows

package config

// applyPlatformDefaults replaces the Linux filesystem-path defaults with
// Windows-appropriate ones rooted at C:\Pterodactyl. These are only defaults:
// values present in config.yml (including the data directory provided by the
// Panel node configuration) are unmarshalled over the top and take priority.
func applyPlatformDefaults(c *Configuration) {
	c.System.RootDirectory = `C:\Pterodactyl`
	c.System.LogDirectory = `C:\Pterodactyl\logs`
	c.System.Data = `C:\Pterodactyl\volumes`
	c.System.ArchiveDirectory = `C:\Pterodactyl\archives`
	c.System.BackupDirectory = `C:\Pterodactyl\backups`
	c.System.TmpDirectory = `C:\Pterodactyl\tmp`
}
