package installer

import (
	"fmt"
	"os"
	"os/exec"

	"orbitron/internal/config"
)

// RunUninstall stops services, parses config to discover custom paths,
// and removes binary, config, logs, storage (including manifests), logrotate config, and system user/group.
func RunUninstall() error {
	euid := os.Geteuid()
	if euid != 0 {
		return fmt.Errorf("uninstallation requires root permissions (current EUID: %d, run with sudo)", euid)
	}

	fmt.Println("🧹 Uninstalling Orbitron...")

	// 1. Read config file to discover dynamic storage/manifest paths before deleting config
	customStorageDir := ""
	if cfg, err := config.LoadConfig(ConfigFile); err == nil {
		if cfg.StoragePath != "" {
			customStorageDir = cfg.StoragePath
		}
	}

	// 2. Stop and disable Systemd service
	fmt.Println("  ℹ Stopping and disabling systemd service...")
	_ = exec.Command("systemctl", "stop", "orbitron").Run()
	_ = exec.Command("systemctl", "disable", "orbitron").Run()
	fmt.Println("  ✔ Systemd service stopped and disabled")

	if err := os.Remove(SystemdFile); err == nil {
		fmt.Printf("  ✔ Removed systemd service file (%s)\n", SystemdFile)
	}
	_ = exec.Command("systemctl", "daemon-reload").Run()
	_ = exec.Command("systemctl", "reset-failed").Run()

	// 3. Remove Logrotate configuration
	if err := os.Remove(LogrotateFile); err == nil {
		fmt.Printf("  ✔ Removed logrotate file (%s)\n", LogrotateFile)
	}

	// 4. Remove Binary
	if err := os.Remove(BinPath); err == nil {
		fmt.Printf("  ✔ Removed binary (%s)\n", BinPath)
	}

	// 5. Remove directories (/etc/orbitron, /var/log/orbitron, default /var/lib/orbitron, and custom storage path)
	dirsToClean := []string{ConfigDir, LogDir, "/var/lib/orbitron"}
	if customStorageDir != "" && customStorageDir != "/var/lib/orbitron" && customStorageDir != "/var/lib/orbitron/storage" {
		dirsToClean = append(dirsToClean, customStorageDir)
	}

	for _, dir := range dirsToClean {
		if err := os.RemoveAll(dir); err == nil {
			fmt.Printf("  ✔ Removed directory (%s)\n", dir)
		}
	}

	// 6. Remove system user and group
	if err := exec.Command("userdel", "orbitron").Run(); err == nil {
		fmt.Println("  ✔ Removed system user 'orbitron'")
	}
	if err := exec.Command("groupdel", "orbitron").Run(); err == nil {
		fmt.Println("  ✔ Removed system group 'orbitron'")
	}

	fmt.Println("\n✨ Orbitron uninstallation completed successfully!")
	return nil
}
