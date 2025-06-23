package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	adbPath     = "platform-tools/adb.exe"
	maxAdbWait  = 10 * time.Second
	commandWait = 30 * time.Second // timeout for individual commands
)

func main() {
	if _, err := os.Stat(adbPath); os.IsNotExist(err) {
		exitError(fmt.Errorf("ADB not found at %s", adbPath))
	}

	if err := startAdbServer(); err != nil {
		exitError(fmt.Errorf("failed to start ADB server: %w", err))
	}

	if !waitForDevice(maxAdbWait) {
		exitError(fmt.Errorf("no device connected — enable USB debugging and reconnect"))
	}

	option := promptOption(
		"Select an option:\n" +
			"1. Backup Android device\n" +
			"2. Copy files/directories from Android device to PC\n" +
			"3. Restore Android device from backup\n" +
			"Enter option number: ")

	switch option {
	case "1":
		backupAndroidDevice()
	case "2":
		copyFilesFromAndroid()
	case "3":
		restoreAndroidDevice()
	default:
		exitError(fmt.Errorf("invalid option: choose 1, 2, or 3"))
	}
}

func exitError(err error) {
	fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	os.Exit(1)
}

func runCommand(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func startAdbServer() error {
	ctx, cancel := context.WithTimeout(context.Background(), commandWait)
	defer cancel()
	return runCommand(ctx, adbPath, "start-server")
}

func waitForDevice(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		output, err := exec.CommandContext(ctx, adbPath, "devices").Output()
		cancel()
		if err == nil {
			for _, line := range strings.Split(string(output), "\n") {
				if strings.Contains(line, "\tdevice") && !strings.HasPrefix(line, "List of devices") {
					return true
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

func verifySourcePath(source string) error {
	ctx, cancel := context.WithTimeout(context.Background(), commandWait)
	defer cancel()
	return runCommand(ctx, adbPath, "shell", "ls", source)
}

func backupAndroidDevice() {
	backupDir := "backups"
	backupFile := fmt.Sprintf("backup_%s.ab", time.Now().Format("20060102_150405"))
	backupPath := filepath.Join(backupDir, backupFile)

	if err := os.MkdirAll(backupDir, 0755); err != nil {
		exitError(fmt.Errorf("failed to create backup directory: %w", err))
	}

	fmt.Printf("Backing up to %s\n", backupPath)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	if err := runCommand(ctx, adbPath, "backup", "-f", backupPath, "-all", "-apk", "-shared"); err != nil {
		exitError(fmt.Errorf("backup failed: %w", err))
	}

	fmt.Println("Backup completed successfully.")
}

func copyFilesFromAndroid() {
	defaultSource := "/storage/emulated/0/"
	source := promptInput(fmt.Sprintf("Enter source path on device [%s]: ", defaultSource))
	if source == "" {
		source = defaultSource
	}

	if err := verifySourcePath(source); err != nil {
		exitError(fmt.Errorf("source path does not exist or is inaccessible: %w", err))
	}

	destDir := filepath.Join("backups", "files", time.Now().Format("20060102_150405"))
	if err := os.MkdirAll(destDir, 0755); err != nil {
		exitError(fmt.Errorf("failed to create local destination directory: %w", err))
	}

	fmt.Printf("Copying from %s to %s\n", source, destDir)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	if err := runCommand(ctx, adbPath, "pull", source, destDir); err != nil {
		exitError(fmt.Errorf("file copy failed: %w", err))
	}

	fmt.Println("Files copied successfully.")
}

func restoreAndroidDevice() {
	backupDir := "backups"
	files, err := listBackupFiles(backupDir)
	if err != nil {
		exitError(err)
	}
	if len(files) == 0 {
		exitError(fmt.Errorf("no backup files found in %s", backupDir))
	}

	fmt.Println("Available backups:")
	for i, f := range files {
		fmt.Printf("%d. %s\n", i+1, f)
	}
	choiceStr := promptInput("Enter number of backup to restore: ")
	index, err := strconv.Atoi(strings.TrimSpace(choiceStr))
	if err != nil || index < 1 || index > len(files) {
		exitError(fmt.Errorf("invalid choice"))
	}

	backupPath := filepath.Join(backupDir, files[index-1])
	fmt.Printf("Restoring from %s\n", backupPath)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	if err := runCommand(ctx, adbPath, "restore", backupPath); err != nil {
		exitError(fmt.Errorf("restore failed: %w", err))
	}

	fmt.Println("Restore completed successfully.")
}

func listBackupFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", dir, err)
	}
	var backups []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".ab") {
			backups = append(backups, e.Name())
		}
	}
	return backups, nil
}

func promptInput(prompt string) string {
	fmt.Print(prompt)
	scanner := bufio.NewScanner(os.Stdin)
	if scanner.Scan() {
		return strings.TrimSpace(scanner.Text())
	}
	return ""
}

func promptOption(prompt string) string {
	return promptInput(prompt)
}
