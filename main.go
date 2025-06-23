package main

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Constants for paths and settings
const (
	adbPath           = "platform-tools/adb.exe"
	backupDir         = "backups"
	defaultBackupName = "backup_%s.ab.gz" // Compressed backup with timestamp
	sourceDir         = "/storage/emulated/0/"
	destDir           = "backups/files"
	retryAttempts     = 3
	retryDelay        = 2 * time.Second
)

// main handles user input and routes to the appropriate function
func main() {
	fmt.Println("Select an option:")
	fmt.Println("1. Backup Android device")
	fmt.Println("2. Copy files/directories from Android device to PC")

	// Use a larger buffer for scanner to handle larger inputs
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("Enter the option number (1 or 2): ")
		if scanner.Scan() {
			option := strings.TrimSpace(scanner.Text())
			switch option {
			case "1":
				if err := backupAndroidDevice(); err != nil {
					fmt.Printf("Backup failed: %v\n", err)
				}
				return
			case "2":
				if err := copyFilesFromAndroid(); err != nil {
					fmt.Printf("File copy failed: %v\n", err)
				}
				return
			default:
				fmt.Println("Invalid option. Please choose 1 or 2.")
			}
		}
	}
}

// backupAndroidDevice creates a compressed backup of the Android device
func backupAndroidDevice() error {
	// Generate backup filename with timestamp
	backupName := fmt.Sprintf(defaultBackupName, time.Now().Format("20060102_150405"))
	backupPath := filepath.Join(backupDir, backupName)

	// Ensure a backup directory exists
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		return fmt.Errorf("failed to create backup directory: %w", err)
	}

	// Check available disk space (basic check)
	if err := checkDiskSpace(backupDir); err != nil {
		return err
	}

	fmt.Printf("Creating compressed backup at %s\n", backupPath)

	// Create a compressed backup file
	file, err := os.Create(backupPath)
	if err != nil {
		return fmt.Errorf("failed to create backup file: %w", err)
	}
	defer func(file *os.File) {
		err := file.Close()
		if err != nil {
			fmt.Printf("Error closing backup file: %v\n", err)
		}
	}(file)

	gzWriter := gzip.NewWriter(file)
	defer func(gzWriter *gzip.Writer) {
		err := gzWriter.Close()
		if err != nil {
			fmt.Printf("Error closing gzip writer: %v\n", err)
		}
	}(gzWriter)

	// Execute ADB backup with retry logic
	cmd := exec.Command(adbPath, "backup", "-all")
	cmd.Stdout = gzWriter
	cmd.Stderr = os.Stderr // Stream errors to the console for real-time feedback

	return runCommandWithRetry(cmd, "backup")
}

// copyFilesFromAndroid transfers files from Android to PC
func copyFilesFromAndroid() error {
	// Ensure destination directory exists
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("failed to create destination directory: %w", err)
	}

	// Check available disk space
	if err := checkDiskSpace(destDir); err != nil {
		return err
	}

	fmt.Printf("Copying files from %s to %s\n", sourceDir, destDir)

	// Execute ADB pull with retry logic
	cmd := exec.Command(adbPath, "pull", sourceDir, destDir)
	cmd.Stderr = os.Stderr // Stream errors for real-time feedback

	return runCommandWithRetry(cmd, "file copy")
}

// runCommandWithRetry executes a command with retry logic
func runCommandWithRetry(cmd *exec.Cmd, operation string) error {
	for attempt := 1; attempt <= retryAttempts; attempt++ {
		fmt.Printf("Attempt %d/%d for %s...\n", attempt, retryAttempts, operation)
		if err := cmd.Run(); err == nil {
			fmt.Printf("%s completed successfully.\n", operation)
			return nil
		} else {
			fmt.Printf("Attempt %d failed: %v\n", attempt, err)
			if attempt < retryAttempts {
				time.Sleep(retryDelay)
			}
		}
	}
	return fmt.Errorf("%s failed after %d attempts", operation, retryAttempts)
}

// checkDiskSpace performs a basic check for available disk space
func checkDiskSpace(dir string) error {
	// This is a simplified check; consider using syscall.States for precise disk stats
	file, err := os.CreateTemp(dir, "diskcheck_*.tmp")
	if err != nil {
		return fmt.Errorf("insufficient disk space or permissions in %s: %w", dir, err)
	}
	defer func(name string) {
		err := os.Remove(name)
		if err != nil {
			fmt.Printf("Error removing temporary file %s: %v\n", name, err)
		}
	}(file.Name())
	defer func(file *os.File) {
		err := file.Close()
		if err != nil {
			fmt.Printf("Error closing temporary file: %v\n", err)
		}
	}(file)

	// Try writing 1MB to test
	data := make([]byte, 1024*1024)
	_, err = file.Write(data)
	if err != nil {
		return fmt.Errorf("failed to write to disk in %s: %w", dir, err)
	}
	return nil
}
