package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	adbPath    = "platform-tools/adb.exe"
	maxAdbWait = 5 * time.Second
)

func main() {
	// Check if ADB exists
	if _, err := os.Stat(adbPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Error: ADB not found at %s\n", adbPath)
		os.Exit(1)
	}

	// Start ADB server
	if err := startAdbServer(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to start ADB server: %v\n", err)
		os.Exit(1)
	}

	// Check for connected device
	if !isDeviceConnected() {
		fmt.Fprintf(os.Stderr, "Error: No device connected. Please connect an Android device with USB debugging enabled.\n")
		os.Exit(1)
	}

	// Prompt user for option
	fmt.Println("Select an option:")
	fmt.Println("1. Backup Android device")
	fmt.Println("2. Copy files/directories from Android device to PC")
	fmt.Println("3. Restore Android device from backup")
	fmt.Print("Enter the option number: ")

	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		fmt.Fprintf(os.Stderr, "Error: Failed to read input\n")
		os.Exit(1)
	}
	option := strings.TrimSpace(scanner.Text())

	switch option {
	case "1":
		backupAndroidDevice()
	case "2":
		copyFilesFromAndroid()
	case "3":
		restoreAndroidDevice()
	default:
		fmt.Fprintf(os.Stderr, "Error: Invalid option. Please choose 1, 2, or 3.\n")
		os.Exit(1)
	}
}

func startAdbServer() error {
	cmd := exec.Command(adbPath, "start-server")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to start ADB server: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	return nil
}

func isDeviceConnected() bool {
	deadline := time.Now().Add(maxAdbWait)
	for time.Now().Before(deadline) {
		cmd := exec.Command(adbPath, "devices")
		output, err := cmd.Output()
		if err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		lines := strings.Split(string(output), "\n")
		for _, line := range lines {
			if strings.Contains(line, "\tdevice") && !strings.Contains(line, "List of devices") {
				return true
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

func verifySourcePath(sourceDir string) error {
	cmd := exec.Command(adbPath, "shell", "ls", sourceDir)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("source directory %s does not exist or is inaccessible", sourceDir)
	}
	return nil
}

func backupAndroidDevice() {
	backupDir := "backups"
	backupName := fmt.Sprintf("backup_%s.ab", time.Now().Format("20060102_150405"))
	backupPath := filepath.Join(backupDir, backupName)

	if err := os.MkdirAll(backupDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to create backup directory: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Creating backup of Android device to %s\n", backupPath)
	cmd := exec.Command(adbPath, "backup", "-f", backupPath, "-all", "-apk", "-shared")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to create backup: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Backup completed and saved to %s\n", backupPath)
}

func copyFilesFromAndroid() {
	sourceDir := "/storage/emulated/0/"
	pcDestinationDir := filepath.Join("backups", "files", time.Now().Format("20060102_150405"))

	if err := verifySourcePath(sourceDir); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	if err := os.MkdirAll(pcDestinationDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to create destination directory: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Copying files from %s to %s\n", sourceDir, pcDestinationDir)
	cmd := exec.Command(adbPath, "pull", sourceDir, pcDestinationDir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to copy files: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Files copied successfully.")
}

func restoreAndroidDevice() {
	backupDir := "backups"
	fmt.Println("Available backup files:")
	backupFiles, err := listBackupFiles(backupDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to list backup files: %v\n", err)
		os.Exit(1)
	}
	if len(backupFiles) == 0 {
		fmt.Fprintf(os.Stderr, "Error: No backup files found in %s\n", backupDir)
		os.Exit(1)
	}

	for i, file := range backupFiles {
		fmt.Printf("%d. %s\n", i+1, file)
	}
	fmt.Print("Enter the number of the backup file to restore: ")

	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		fmt.Fprintf(os.Stderr, "Error: Failed to read input\n")
		os.Exit(1)
	}
	choice := strings.TrimSpace(scanner.Text())
	index, err := parseChoice(choice, len(backupFiles))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	backupPath := filepath.Join(backupDir, backupFiles[index-1])
	if _, err := os.Stat(backupPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Error: Backup file %s does not exist\n", backupPath)
		os.Exit(1)
	}

	fmt.Printf("Restoring Android device from %s\n", backupPath)
	cmd := exec.Command(adbPath, "restore", backupPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to restore backup: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Restore completed successfully.")
}

func listBackupFiles(dir string) ([]string, error) {
	var backups []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to read directory %s: %v", dir, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".ab") {
			backups = append(backups, entry.Name())
		}
	}
	return backups, nil
}

func parseChoice(choice string, max int) (int, error) {
	n, err := fmt.Sscanf(choice, "%d", &choice)
	if err != nil || n != 1 {
		return 0, fmt.Errorf("invalid input: please enter a number")
	}
	if choiceInt, err := strconv.Atoi(choice); err == nil {
		if choiceInt < 1 || choiceInt > max {
			return 0, fmt.Errorf("invalid choice: please select a number between 1 and %d", max)
		}
		return choiceInt, nil
	}
	return 0, fmt.Errorf("invalid input: please enter a number")
}
