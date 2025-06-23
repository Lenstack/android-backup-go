package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config holds program-wide configuration.
type Config struct {
	AdbPath      string
	BackupDir    string
	TimeoutShort time.Duration
	TimeoutLong  time.Duration
}

// NewConfig initializes the configuration with defaults and parsed flags.
func NewConfig() *Config {
	cfg := &Config{
		AdbPath:      "platform-tools/adb.exe",
		BackupDir:    "backups",
		TimeoutShort: 10 * time.Second,
		TimeoutLong:  2 * time.Hour,
	}
	flag.StringVar(&cfg.AdbPath, "adb", cfg.AdbPath, "path to ADB executable")
	flag.Parse()
	return cfg
}

// AdbCommand represents an ADB command with arguments and timeout.
type AdbCommand struct {
	Args    []string
	Timeout time.Duration
}

// run executes an ADB command with the given context and configuration.
func (cfg *Config) run(cmd AdbCommand) error {
	ctx, cancel := context.WithTimeout(context.Background(), cmd.Timeout)
	defer cancel()
	adbCmd := exec.CommandContext(ctx, cfg.AdbPath, cmd.Args...)
	adbCmd.Stdout = os.Stdout
	adbCmd.Stderr = os.Stderr
	if err := adbCmd.Run(); err != nil {
		return fmt.Errorf("ADB command %v failed: %w", cmd.Args, err)
	}
	return nil
}

// checkAdb validates the ADB executable path.
func (cfg *Config) checkAdb() error {
	if _, err := os.Stat(cfg.AdbPath); os.IsNotExist(err) {
		return fmt.Errorf("ADB executable not found at %s", cfg.AdbPath)
	}
	return nil
}

// startAdbServer starts the ADB server.
func (cfg *Config) startAdbServer() error {
	return cfg.run(AdbCommand{Args: []string{"start-server"}, Timeout: cfg.TimeoutShort})
}

// waitForDevice waits for an ADB device to be connected.
func (cfg *Config) waitForDevice() error {
	return cfg.run(AdbCommand{Args: []string{"wait-for-device"}, Timeout: cfg.TimeoutShort})
}

// backupDevice creates a backup of the Android device.
func (cfg *Config) backupDevice() error {
	if err := os.MkdirAll(cfg.BackupDir, 0o755); err != nil {
		return fmt.Errorf("failed to create backup directory: %w", err)
	}
	backupFile := filepath.Join(cfg.BackupDir, fmt.Sprintf("backup_%s.ab", time.Now().Format("20060102_150405")))
	fmt.Printf("Backing up to %s\n", backupFile)
	return cfg.run(AdbCommand{
		Args:    []string{"backup", "-f", backupFile, "-all", "-apk", "-shared"},
		Timeout: cfg.TimeoutLong,
	})
}

// copyFromDevice copies files from the device to the local machine.
func (cfg *Config) copyFromDevice() error {
	const defaultSrc = "/storage/emulated/0/"
	src := prompt(fmt.Sprintf("Enter source path on device [%s]: ", defaultSrc))
	if src == "" {
		src = defaultSrc
	}
	// Validate source path
	if err := cfg.run(AdbCommand{Args: []string{"shell", "ls", src}, Timeout: cfg.TimeoutShort}); err != nil {
		return fmt.Errorf("invalid source path %s: %w", src, err)
	}
	dst := filepath.Join(cfg.BackupDir, "files", time.Now().Format("20060102_150405"))
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("failed to create destination directory: %w", err)
	}
	fmt.Printf("Copying from %s to %s\n", src, dst)
	return cfg.run(AdbCommand{Args: []string{"pull", src, dst}, Timeout: cfg.TimeoutLong})
}

// restoreDevice restores a backup to the Android device.
func (cfg *Config) restoreDevice() error {
	files, err := filepath.Glob(filepath.Join(cfg.BackupDir, "*.ab"))
	if err != nil || len(files) == 0 {
		return fmt.Errorf("no backup files found in %s: %w", cfg.BackupDir, err)
	}
	for i, f := range files {
		fmt.Printf("%d) %s\n", i+1, filepath.Base(f))
	}
	choice := prompt("Enter number of backup to restore: ")
	i, err := strconv.Atoi(choice)
	if err != nil || i < 1 || i > len(files) {
		return errors.New("invalid backup selection")
	}
	backupFile := files[i-1]
	fmt.Printf("Restoring from %s\n", backupFile)
	return cfg.run(AdbCommand{Args: []string{"restore", backupFile}, Timeout: cfg.TimeoutLong})
}

// prompt reads a trimmed string from stdin with the given message.
func prompt(msg string) string {
	fmt.Print(msg)
	scanner := bufio.NewScanner(os.Stdin)
	if scanner.Scan() {
		return strings.TrimSpace(scanner.Text())
	}
	return ""
}

// chooseOption displays a menu and returns the user's selection.
func chooseOption(header string, options []string) (int, error) {
	fmt.Println(header)
	for i, opt := range options {
		fmt.Printf("%d) %s\n", i+1, opt)
	}
	for {
		input := prompt("Enter choice: ")
		choice, err := strconv.Atoi(input)
		if err == nil && choice >= 1 && choice <= len(options) {
			return choice, nil
		}
		fmt.Println("Invalid choice, try again.")
	}
}

// exitError prints an error to stderr and exits with status code 1.
func exitError(err error) {
	fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	os.Exit(1)
}

func main() {
	cfg := NewConfig()

	if err := cfg.checkAdb(); err != nil {
		exitError(err)
	}
	if err := cfg.startAdbServer(); err != nil {
		exitError(err)
	}
	if err := cfg.waitForDevice(); err != nil {
		exitError(errors.New("no device connected: enable USB debugging and reconnect"))
	}

	choice, err := chooseOption("Select an option:", []string{
		"Backup Android device",
		"Copy files/directories from device to PC",
		"Restore Android device from backup",
	})
	if err != nil {
		exitError(err)
	}

	var actionErr error
	switch choice {
	case 1:
		actionErr = cfg.backupDevice()
	case 2:
		actionErr = cfg.copyFromDevice()
	case 3:
		actionErr = cfg.restoreDevice()
	}
	if actionErr != nil {
		exitError(actionErr)
	}
}
