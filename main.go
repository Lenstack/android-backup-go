package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const adbPath = "platform-tools/adb.exe"

func main() {
	// Prompt the user to choose between backup and file copy
	fmt.Println("Select an option:")
	fmt.Println("1. Backup Android device")
	fmt.Println("2. Copy files/directories from Android device to PC")
	fmt.Print("Enter the option number: ")

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Scan()
	option := strings.TrimSpace(scanner.Text())

	switch option {
	case "1":
		backupAndroidDevice()
	case "2":
		copyFilesFromAndroid()
	default:
		fmt.Println("Invalid option. Please choose 1 or 2.")
	}
}

func backupAndroidDevice() {
	// Define paths for backup and PC transfer
	backupDir := "backups"
	backupName := "backup.ab"
	backupPath := filepath.Join(backupDir, backupName)

	fmt.Println("Creating backup of Android device")
	// Create a backup using ADB
	adbCommand := exec.Command(adbPath, "backup", "-f", backupPath, "-all")

	if err := adbCommand.Run(); err != nil {
		fmt.Printf("Error creating backup: %v\n", err)
		return
	}

	fmt.Printf("Backup completed and saved to %s\n", backupPath)
}

func copyFilesFromAndroid() {
	// Define the source directory on your Android device and the destination directory on your PC
	sourceDir := "/storage/emulated/0/" // Change this to the source directory on your Android device
	pcDestinationDir := "backups/files"

	// Create the PC destination directory if it doesn't exist
	if err := os.MkdirAll(pcDestinationDir, os.ModePerm); err != nil {
		fmt.Printf("Error creating PC destination directory: %v\n", err)
		return
	}

	fmt.Printf("Copying files from %s to %s\n", sourceDir, pcDestinationDir)
	// Use ADB to copy files/directories from Android to PC
	adbCommand := exec.Command(adbPath, "pull", sourceDir, pcDestinationDir)

	if err := adbCommand.Run(); err != nil {
		fmt.Printf("Error copying files from Android to PC: %v\n", err)
		return
	}

	fmt.Printf("Files copied from Android to PC successfully.\n")
}
