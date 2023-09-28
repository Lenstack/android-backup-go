package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

const adbPath = "adb-manager/adb.exe"

func main() {
	// Define paths for backup and PC transfer
	backupDir := "."
	pcTransferDir := "backups"

	// Create a backup using ADB
	backupName := "backup.ab"
	backupPath := filepath.Join(backupDir, backupName)

	adbCommand := exec.Command(adbPath, "backup", "-f", backupPath, "-all")

	if err := adbCommand.Run(); err != nil {
		fmt.Printf("Error creating backup: %v\n", err)
		return
	}

	// Transfer the backup to the PC
	pcBackupPath := filepath.Join(pcTransferDir, backupName)
	if err := os.MkdirAll(pcTransferDir, os.ModePerm); err != nil {
		fmt.Printf("Error creating PC transfer directory: %v\n", err)
		return
	}

	if err := os.Rename(backupPath, pcBackupPath); err != nil {
		fmt.Printf("Error moving backup to PC: %v\n", err)
		return
	}

	fmt.Printf("Backup completed and saved to %s\n", pcBackupPath)
}
