package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config holds program-wide configuration.
type Config struct {
	AdbPath       string
	BackupDir     string
	TimeoutShort  time.Duration
	TimeoutLong   time.Duration
	LogFile       string
	Concurrency   int
	MaxRetries    int
	RetryDelay    time.Duration
	BackoffFactor float64
}

// Cache manages failed operations with retry tracking and logging.
type Cache struct {
	failedOps  map[string]int // Operation key -> retry count
	maxRetries int
	mutex      sync.RWMutex
	logFile    *os.File
}

// NewCache initializes the failure cache with logging.
func NewCache(logPath string, maxRetries int) (*Cache, error) {
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("failed to open log file: %w", err)
	}
	return &Cache{
		failedOps:  make(map[string]int),
		maxRetries: maxRetries,
		logFile:    logFile,
	}, nil
}

// Close closes the log file.
func (c *Cache) Close() error {
	return c.logFile.Close()
}

// AddFailed adds a failed operation to the cache and logs it with type and error.
func (c *Cache) AddFailed(key, opType string, attempt int, err error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.failedOps[key] = attempt
	if attempt <= c.maxRetries {
		fmt.Fprintf(c.logFile, "[%s] Type: %s, Key: %s, Attempt: %d, Error: %v\n",
			time.Now().Format(time.RFC3339), opType, key, attempt, err)
	}
}

// ShouldSkip checks if an operation should be skipped based on retry count.
func (c *Cache) ShouldSkip(key string) bool {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	return c.failedOps[key] >= c.maxRetries
}

// GetFailed returns a list of failed operations for retry.
func (c *Cache) GetFailed() []string {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	var failed []string
	for key, count := range c.failedOps {
		if count > 0 && count <= c.maxRetries {
			failed = append(failed, key)
		}
	}
	return failed
}

// NewConfig initializes the configuration with defaults and parsed flags.
func NewConfig() *Config {
	cfg := &Config{
		AdbPath:       "platform-tools/adb.exe",
		BackupDir:     "backups",
		TimeoutShort:  10 * time.Second,
		TimeoutLong:   2 * time.Hour,
		LogFile:       "backups/backup_errors.log",
		Concurrency:   runtime.NumCPU(),
		MaxRetries:    1,
		RetryDelay:    1 * time.Second,
		BackoffFactor: 2.0,
	}
	flag.StringVar(&cfg.AdbPath, "adb", cfg.AdbPath, "path to ADB executable")
	flag.StringVar(&cfg.BackupDir, "backup-dir", cfg.BackupDir, "backup directory")
	flag.StringVar(&cfg.LogFile, "log", cfg.LogFile, "log file for failed operations")
	flag.IntVar(&cfg.Concurrency, "concurrency", cfg.Concurrency, "number of concurrent operations")
	flag.IntVar(&cfg.MaxRetries, "retries", cfg.MaxRetries, "maximum retry attempts for failed operations")
	flag.DurationVar(&cfg.RetryDelay, "retry-delay", cfg.RetryDelay, "initial delay between retries")
	flag.Float64Var(&cfg.BackoffFactor, "backoff-factor", cfg.BackoffFactor, "exponential backoff factor")
	flag.Parse()
	return cfg
}

// AdbCommand represents an ADB command with arguments and timeout.
type AdbCommand struct {
	Args    []string
	Timeout time.Duration
	Key     string // Unique identifier for caching (e.g., file path or command description)
	Type    string // Operation type ("File" or "Command")
}

// run executes an ADB command with retries and exponential backoff.
func (cfg *Config) run(cmd AdbCommand, cache *Cache) error {
	var lastErr error
	for attempt := 1; attempt <= cfg.MaxRetries+1; attempt++ {
		if cache != nil && cache.ShouldSkip(cmd.Key) {
			return fmt.Errorf("skipped %s after %d retries", cmd.Key, cfg.MaxRetries)
		}

		ctx, cancel := context.WithTimeout(context.Background(), cmd.Timeout)
		defer cancel()
		adbCmd := exec.CommandContext(ctx, cfg.AdbPath, cmd.Args...)
		adbCmd.Stdout = os.Stdout
		adbCmd.Stderr = os.Stderr
		err := adbCmd.Run()
		if err == nil {
			return nil // Success
		}

		lastErr = fmt.Errorf("ADB command %v failed: %w", cmd.Args, err)
		if cache != nil {
			cache.AddFailed(cmd.Key, cmd.Type, attempt, lastErr)
		}
		fmt.Fprintf(os.Stderr, "Attempt %d/%d failed for %s: %v\n", attempt, cfg.MaxRetries, cmd.Key, err)

		// Apply exponential backoff
		if attempt <= cfg.MaxRetries {
			delay := float64(cfg.RetryDelay) * math.Pow(cfg.BackoffFactor, float64(attempt-1))
			time.Sleep(time.Duration(delay))
		}
	}
	return lastErr // Return last error if all retries fail
}

// checkAdb validates the ADB executable path.
func (cfg *Config) checkAdb() error {
	if _, err := os.Stat(cfg.AdbPath); os.IsNotExist(err) {
		return fmt.Errorf("ADB executable not found at %s", cfg.AdbPath)
	}
	return nil
}

// startAdbServer starts the ADB server with retries.
func (cfg *Config) startAdbServer(cache *Cache) error {
	return cfg.run(AdbCommand{
		Args:    []string{"start-server"},
		Timeout: cfg.TimeoutShort,
		Key:     "start-server",
		Type:    "Command",
	}, cache)
}

// waitForDevice waits for an ADB device to be connected with retries.
func (cfg *Config) waitForDevice(cache *Cache) error {
	return cfg.run(AdbCommand{
		Args:    []string{"wait-for-device"},
		Timeout: cfg.TimeoutShort,
		Key:     "wait-for-device",
		Type:    "Command",
	}, cache)
}

// backupDevice creates a backup of the Android device with retries.
func (cfg *Config) backupDevice(cache *Cache) error {
	if err := os.MkdirAll(cfg.BackupDir, 0o755); err != nil {
		return fmt.Errorf("failed to create backup directory: %w", err)
	}
	backupFile := filepath.Join(cfg.BackupDir, fmt.Sprintf("backup_%s.ab", time.Now().Format("20060102_150405")))
	fmt.Printf("Backing up to %s\n", backupFile)
	return cfg.run(AdbCommand{
		Args:    []string{"backup", "-f", backupFile, "-all", "-apk", "-shared"},
		Timeout: cfg.TimeoutLong,
		Key:     backupFile,
		Type:    "File",
	}, cache)
}

// listFiles lists files in a directory on the device with retries.
func (cfg *Config) listFiles(src string, cache *Cache) ([]string, error) {
	var files []string
	err := cfg.run(AdbCommand{
		Args:    []string{"shell", "find", src, "-type", "f"},
		Timeout: cfg.TimeoutShort,
		Key:     "list-files-" + src,
		Type:    "Command",
	}, cache)
	if err != nil {
		return nil, fmt.Errorf("failed to list files in %s: %w", src, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.TimeoutShort)
	defer cancel()
	cmd := exec.CommandContext(ctx, cfg.AdbPath, "shell", "find", src, "-type", "f")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to capture file list in %s: %w", src, err)
	}

	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		file := strings.TrimSpace(scanner.Text())
		if file != "" {
			files = append(files, file)
		}
	}
	return files, nil
}

// copyFile pulls a single file from the device with retry logic.
func (cfg *Config) copyFile(src, dst string, cache *Cache, wg *sync.WaitGroup, sem chan struct{}) error {
	defer wg.Done()
	defer func() { <-sem }() // Release semaphore

	dstPath := filepath.Join(dst, strings.ReplaceAll(src, "/", "_"))
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		cache.AddFailed(src, "File", 1, err)
		return fmt.Errorf("failed to create destination directory for %s: %w", src, err)
	}

	fmt.Printf("Copying %s to %s\n", src, dstPath)
	err := cfg.run(AdbCommand{
		Args:    []string{"pull", src, dstPath},
		Timeout: cfg.TimeoutLong,
		Key:     src,
		Type:    "File",
	}, cache)
	if err != nil {
		return fmt.Errorf("failed to copy %s: %w", src, err)
	}
	return nil
}

// retryFailedFiles retries failed file copies.
func (cfg *Config) retryFailedFiles(dst string, cache *Cache) error {
	failed := cache.GetFailed()
	if len(failed) == 0 {
		fmt.Println("No files to retry.")
		return nil
	}

	fmt.Printf("Retrying %d failed files\n", len(failed))
	var wg sync.WaitGroup
	sem := make(chan struct{}, cfg.Concurrency)

	for _, src := range failed {
		sem <- struct{}{} // Acquire semaphore
		wg.Add(1)
		go cfg.copyFile(src, dst, cache, &wg, sem)
	}

	wg.Wait()
	return nil
}

// copyFromDevice copies files from the device to the local machine with concurrency and retries.
func (cfg *Config) copyFromDevice(cache *Cache) error {
	const defaultSrc = "/storage/emulated/0/"
	src := prompt(fmt.Sprintf("Enter source path on device [%s]: ", defaultSrc))
	if src == "" {
		src = defaultSrc
	}

	// Validate source path with retries
	err := cfg.run(AdbCommand{
		Args:    []string{"shell", "ls", src},
		Timeout: cfg.TimeoutShort,
		Key:     "validate-" + src,
		Type:    "Command",
	}, cache)
	if err != nil {
		return fmt.Errorf("invalid source path %s: %w", src, err)
	}

	dst := filepath.Join(cfg.BackupDir, "files", time.Now().Format("20060102_150405"))
	// List files with retries
	files, err := cfg.listFiles(src, cache)
	if err != nil {
		return fmt.Errorf("failed to list files: %w", err)
	}

	// Copy files concurrently
	var wg sync.WaitGroup
	sem := make(chan struct{}, cfg.Concurrency)
	for _, file := range files {
		sem <- struct{}{} // Acquire semaphore
		wg.Add(1)
		go cfg.copyFile(file, dst, cache, &wg, sem)
	}
	wg.Wait()

	// Retry failed files
	if err := cfg.retryFailedFiles(dst, cache); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: retry phase had issues: %v\n", err)
	}

	return nil
}

// restoreDevice restores a backup to the Android device with retries.
func (cfg *Config) restoreDevice(cache *Cache) error {
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
	return cfg.run(AdbCommand{
		Args:    []string{"restore", backupFile},
		Timeout: cfg.TimeoutLong,
		Key:     backupFile,
		Type:    "File",
	}, cache)
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
	fmt.Fprintf(os.Stderr, "Fatal error: %v\n", err)
	os.Exit(1)
}

func main() {
	cfg := NewConfig()
	cache, err := NewCache(cfg.LogFile, cfg.MaxRetries)
	if err != nil {
		exitError(err)
	}
	defer cache.Close()

	if err := cfg.checkAdb(); err != nil {
		exitError(err)
	}
	if err := cfg.startAdbServer(cache); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to start ADB server: %v\n", err)
	}
	if err := cfg.waitForDevice(cache); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: no device connected: %v\n", err)
	}

	choice, err := chooseOption("Select an option:", []string{
		"Backup Android device",
		"Copy files/directories from device to PC",
		"Restore Android device from backup",
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: invalid option selection: %v\n", err)
		return
	}

	var actionErr error
	switch choice {
	case 1:
		actionErr = cfg.backupDevice(cache)
	case 2:
		actionErr = cfg.copyFromDevice(cache)
	case 3:
		actionErr = cfg.restoreDevice(cache)
	}
	if actionErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: operation completed with errors: %v\n", actionErr)
		fmt.Println("Check logs at", cfg.LogFile, "for details on failed operations.")
	} else {
		fmt.Println("Operation completed successfully.")
	}
}
