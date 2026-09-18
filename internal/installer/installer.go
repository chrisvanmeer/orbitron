package installer

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"

	"orbitron/internal/config"
	"orbitron/internal/web"
)

const (
	BinPath       = "/usr/local/bin/orbitron"
	ConfigDir     = "/etc/orbitron"
	ConfigFile    = "/etc/orbitron/config.yml"
	LogDir        = "/var/log/orbitron"
	StorageDir    = "/var/lib/orbitron/storage"
	WebDir        = "/var/lib/orbitron/web"
	SystemdFile   = "/etc/systemd/system/orbitron.service"
	LogrotateDir  = "/etc/logrotate.d"
	LogrotateFile = "/etc/logrotate.d/orbitron"
)

// RunInstall handles user creation, binary self-installation, service setup, config, and logrotate configuration
func RunInstall() error {
	euid := os.Geteuid()
	if euid != 0 {
		return fmt.Errorf("installation requires root permissions (current EUID: %d, run with sudo)", euid)
	}

	fmt.Println("🚀 Installing Orbitron...")

	// 1. Create system group and user
	uid, gid, err := ensureSystemUserAndGroup()
	if err != nil {
		return fmt.Errorf("failed to setup system user: %w", err)
	}
	fmt.Println("  ✔ System user & group 'orbitron' configured")

	// 2. Check for Git dependency
	if _, err := exec.LookPath("git"); err != nil {
		fmt.Println("  ⚠️  Warning: 'git' binary not found in PATH. Install git to support Git-based roles/collections.")
	} else {
		fmt.Println("  ✔ Dependency 'git' detected")
	}

	// 3. Copy binary to /usr/local/bin
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %w", err)
	}

	if err := copyFile(execPath, BinPath, 0755); err != nil {
		return fmt.Errorf("failed to copy binary to %s: %w", BinPath, err)
	}
	_ = os.Chown(BinPath, uid, gid)
	fmt.Printf("  ✔ Installed binary to %s (owned by orbitron:orbitron)\n", BinPath)

	// 4. Create directories with strict 0750 permissions
	baseDirs := []string{"/var/lib/orbitron"}
	dirs := []string{ConfigDir, LogDir, StorageDir, WebDir}

	for _, dir := range append(baseDirs, dirs...) {
		if err := os.MkdirAll(dir, 0750); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
		if err := os.Chmod(dir, 0750); err != nil {
			return fmt.Errorf("failed to set permissions on directory %s: %w", dir, err)
		}
		if err := chownRecursive(dir, uid, gid); err != nil {
			return fmt.Errorf("failed to chown directory %s: %w", dir, err)
		}
	}
	fmt.Println("  ✔ Configured application directories (/etc/orbitron, /var/log/orbitron, /var/lib/orbitron) with 0750 permissions")

	// 5. Extract embedded Web UI assets (HTMX 4.0.0)
	htmxPath := filepath.Join(WebDir, "htmx.min.js")
	if err := os.WriteFile(htmxPath, web.HtmxJS, 0640); err != nil {
		return fmt.Errorf("failed to write embedded htmx.js: %w", err)
	}
	_ = os.Chown(htmxPath, uid, gid)
	fmt.Println("  ✔ Extracted embedded Web UI assets (HTMX 4.0.0)")

	// 6. Create example config.yml if missing with 0640 permissions
	if _, err := os.Stat(ConfigFile); os.IsNotExist(err) {
		if err := os.WriteFile(ConfigFile, []byte(config.GetDefaultConfigYML()), 0640); err != nil {
			return fmt.Errorf("failed to write config file: %w", err)
		}
		_ = os.Chown(ConfigFile, uid, gid)
		fmt.Printf("  ✔ Created configuration file at %s (0640)\n", ConfigFile)
	} else {
		fmt.Printf("  ℹ Configuration file already exists at %s (skipping)\n", ConfigFile)
	}

	// 7. Create Systemd Service File
	systemdContent := `[Unit]
Description=Orbitron Ansible Galaxy Mirror Daemon
After=network.target

[Service]
Type=simple
User=orbitron
Group=orbitron
ExecStart=/usr/local/bin/orbitron --config /etc/orbitron/config.yml
Restart=always
RestartSec=5s

[Install]
WantedBy=multi-user.target
`
	if err := os.WriteFile(SystemdFile, []byte(systemdContent), 0644); err != nil {
		return fmt.Errorf("failed to write systemd unit: %w", err)
	}
	fmt.Printf("  ✔ Created systemd service at %s (User=orbitron)\n", SystemdFile)

	// 8. Configure Logrotate if directory exists
	if _, err := os.Stat(LogrotateDir); !os.IsNotExist(err) {
		logrotateContent := `/var/log/orbitron/*.log {
    daily
    missingok
    rotate 14
    compress
    delaycompress
    notifempty
    create 0640 orbitron orbitron
    postrotate
        systemctl reload orbitron > /dev/null 2>&1 || true
    endscript
}
`
		if err := os.WriteFile(LogrotateFile, []byte(logrotateContent), 0644); err != nil {
			return fmt.Errorf("failed to write logrotate file: %w", err)
		}
		fmt.Printf("  ✔ Configured logrotate rules at %s\n", LogrotateFile)
	}

	// 9. Reload systemd, enable and start service
	_ = exec.Command("systemctl", "daemon-reload").Run()

	if err := exec.Command("systemctl", "enable", "orbitron").Run(); err != nil {
		return fmt.Errorf("failed to enable systemd service: %w", err)
	}
	fmt.Println("  ✔ Enabled systemd service (starts on boot)")

	if err := exec.Command("systemctl", "start", "orbitron").Run(); err != nil {
		return fmt.Errorf("failed to start systemd service: %w", err)
	}
	fmt.Println("  ✔ Started systemd service")

	fmt.Println("\n✨ Orbitron installation completed successfully!")
	return nil
}

func ensureSystemUserAndGroup() (int, int, error) {
	if err := exec.Command("getent", "group", "orbitron").Run(); err != nil {
		cmd := exec.Command("groupadd", "--system", "orbitron")
		if out, err := cmd.CombinedOutput(); err != nil {
			return 0, 0, fmt.Errorf("failed to create group orbitron (%v): %s", err, string(out))
		}
	}

	if err := exec.Command("id", "-u", "orbitron").Run(); err != nil {
		cmd := exec.Command("useradd", "--system", "--gid", "orbitron", "--home-dir", "/var/lib/orbitron", "--shell", "/bin/false", "orbitron")
		if out, err := cmd.CombinedOutput(); err != nil {
			return 0, 0, fmt.Errorf("failed to create user orbitron (%v): %s", err, string(out))
		}
	}

	u, err := user.Lookup("orbitron")
	if err != nil {
		return 0, 0, err
	}

	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, 0, err
	}

	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return 0, 0, err
	}

	return uid, gid, nil
}

func chownRecursive(path string, uid, gid int) error {
	return filepath.Walk(path, func(name string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Chown(name, uid, gid)
	})
}

// copyFile copies src to dst atomically: the content is written to a temporary
// file in the same directory and then renamed over dst. Renaming replaces the
// destination even when it is currently executing, which an open-for-truncate
// would refuse with "text file busy" (ETXTBSY). The destination inherits mode.
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	return os.Rename(tmpName, dst)
}
