package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config holds program-wide configuration.
type Config struct {
	AdbPath        string
	BackupDir      string
	TimeoutList    time.Duration
	TimeoutCopy    time.Duration
	TimeoutCleanup time.Duration
	LogFile        string
	SkipListFile   string
	Concurrency    int
	MaxRetries     int
	RetryDelay     time.Duration
	BackoffFactor  float64
	BatchSize      int // Number of files per batch for copying
	MaxConcurrency int // Maximum concurrency for adaptive adjustment
}

// Trie represents a prefix trie for skip list lookups.
type Trie struct {
	isEnd bool
	nodes map[rune]*Trie
}

func NewTrie() *Trie {
	return &Trie{nodes: make(map[rune]*Trie)}
}

func (t *Trie) Insert(path string) {
	node := t
	for _, r := range path {
		if node.nodes[r] == nil {
			node.nodes[r] = NewTrie()
		}
		node = node.nodes[r]
	}
	node.isEnd = true
}

func (t *Trie) HasPrefix(path string) bool {
	node := t
	for _, r := range path {
		if node.nodes[r] == nil {
			return node.isEnd
		}
		node = node.nodes[r]
	}
	return true
}

// FileStatus represents the status of a file (success or error).
type FileStatus struct {
	Path  string
	Error bool
}

// Cache manages failed operations, file statuses, and skip list.
type Cache struct {
	failedOps     map[string]int        // Operation key -> retry count
	fileStatus    map[string]FileStatus // File path -> status
	maxRetries    int
	mutex         sync.RWMutex
	logFile       *os.File
	logWriter     *bufio.Writer
	logBuffer     []string // In-memory log buffer
	skipFile      *os.File
	skipWriter    *bufio.Writer
	skipTrie      *Trie    // Trie for skip list
	bufferedSkips []string // Buffer for batched skip list updates
}

// NewCache initializes the failure cache with logging and skip list.
func NewCache(logPath, skipPath string, maxRetries int) (*Cache, error) {
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("failed to open log file %s: %w (check file permissions or run as administrator)", logPath, err)
	}
	skipFile, err := os.OpenFile(skipPath, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		logFile.Close()
		return nil, fmt.Errorf("failed to open skip list file %s: %w (check file permissions or run as administrator)", skipPath, err)
	}
	cache := &Cache{
		failedOps:     make(map[string]int),
		fileStatus:    make(map[string]FileStatus),
		maxRetries:    maxRetries,
		logFile:       logFile,
		logWriter:     bufio.NewWriter(logFile),
		logBuffer:     make([]string, 0, 1000),
		skipFile:      skipFile,
		skipWriter:    bufio.NewWriter(skipFile),
		skipTrie:      NewTrie(),
		bufferedSkips: make([]string, 0, 100),
	}
	if err := cache.loadSkipList(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load skip list from %s: %v (continuing with empty skip list; check file permissions or run as administrator)\n", skipPath, err)
	}
	return cache, nil
}

// loadSkipList reads the skip list file into the trie.
func (c *Cache) loadSkipList() error {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if _, err := c.skipFile.Seek(0, 0); err != nil {
		return fmt.Errorf("failed to seek skip list file: %w (check file permissions or file locks)", err)
	}
	scanner := bufio.NewScanner(c.skipFile)
	for scanner.Scan() {
		path := strings.TrimSpace(scanner.Text())
		if path != "" {
			c.skipTrie.Insert(path)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("failed to read skip list file: %w (check file permissions, file locks, or run as administrator)", err)
	}
	return nil
}

// flushLogBuffer writes and clears the log buffer.
func (c *Cache) flushLogBuffer() error {
	if len(c.logBuffer) == 0 {
		return nil
	}
	if _, err := c.logWriter.WriteString(strings.Join(c.logBuffer, "")); err != nil {
		return fmt.Errorf("failed to write log buffer: %w", err)
	}
	c.logBuffer = c.logBuffer[:0] // Clear buffer
	return c.logWriter.Flush()
}

// flushSkips writes buffered skip list entries to trie and file.
func (c *Cache) flushSkips() {
	for _, path := range c.bufferedSkips {
		c.skipTrie.Insert(path)
		c.skipWriter.WriteString(path + "\n")
	}
	c.bufferedSkips = c.bufferedSkips[:0]
}

// Close flushes buffers and closes files.
func (c *Cache) Close() error {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.flushSkips()
	if err := c.flushLogBuffer(); err != nil {
		return err
	}
	if err := c.skipWriter.Flush(); err != nil {
		return fmt.Errorf("failed to flush skip writer: %w", err)
	}
	logErr := c.logFile.Close()
	skipErr := c.skipFile.Close()
	if logErr != nil {
		return fmt.Errorf("failed to close log file: %w", logErr)
	}
	if skipErr != nil {
		return fmt.Errorf("failed to close skip list file: %w", skipErr)
	}
	return nil
}

// AddFailed adds a failed operation to the cache and logs it.
func (c *Cache) AddFailed(key, opType string, attempt int, err error, stderr string) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.failedOps[key] = attempt
	if attempt <= c.maxRetries {
		logEntry := fmt.Sprintf("[%s] Type: %s, Key: %s, Attempt: %d, Error: %v",
			time.Now().Format(time.RFC3339), opType, key, attempt, err)
		if stderr != "" {
			logEntry += fmt.Sprintf(", Stderr: %s", strings.TrimSpace(stderr))
		}
		if attempt == c.maxRetries {
			logEntry += ", Status: Skipped"
		}
		logEntry += "\n"
		c.logBuffer = append(c.logBuffer, logEntry)
		if len(c.logBuffer) >= 1000 {
			if err := c.flushLogBuffer(); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to flush log buffer: %v\n", err)
			}
		}
	}
}

// AddFileStatus caches a file's status and logs errors.
func (c *Cache) AddFileStatus(file FileStatus) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.fileStatus[file.Path] = file
	if file.Error {
		c.bufferedSkips = append(c.bufferedSkips, file.Path)
		logEntry := fmt.Sprintf("[%s] Type: FileError, Path: %s, Status: Permission Denied\n",
			time.Now().Format(time.RFC3339), file.Path)
		c.logBuffer = append(c.logBuffer, logEntry)
		if len(c.bufferedSkips) >= 100 {
			c.flushSkips()
		}
		if len(c.logBuffer) >= 1000 {
			if err := c.flushLogBuffer(); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to flush log buffer: %v\n", err)
			}
		}
	}
}

// IsPathInSkipList checks if a path is in the skip list using the trie.
func (c *Cache) IsPathInSkipList(path string) bool {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	return c.skipTrie.HasPrefix(path)
}

// ShouldSkip checks if an operation should be skipped.
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
		AdbPath:        "platform-tools/adb.exe",
		BackupDir:      "backups",
		TimeoutList:    30 * time.Second,
		TimeoutCopy:    2 * time.Hour,
		TimeoutCleanup: 10 * time.Second,
		LogFile:        "backup_errors.log",
		SkipListFile:   "skip_list.txt",
		Concurrency:    10,
		MaxRetries:     3,
		RetryDelay:     1 * time.Second,
		BackoffFactor:  2.0,
		BatchSize:      100,
		MaxConcurrency: 20,
	}
	flag.StringVar(&cfg.AdbPath, "adb", cfg.AdbPath, "path to ADB executable")
	flag.StringVar(&cfg.BackupDir, "backup-dir", cfg.BackupDir, "backup directory")
	flag.StringVar(&cfg.LogFile, "log", cfg.LogFile, "log file for failed operations")
	flag.StringVar(&cfg.SkipListFile, "skip-list", cfg.SkipListFile, "file for skipped paths")
	flag.IntVar(&cfg.Concurrency, "concurrency", cfg.Concurrency, "initial number of concurrent operations")
	flag.IntVar(&cfg.MaxRetries, "retries", cfg.MaxRetries, "maximum retry attempts")
	flag.DurationVar(&cfg.RetryDelay, "retry-delay", cfg.RetryDelay, "initial delay between retries")
	flag.Float64Var(&cfg.BackoffFactor, "backoff-factor", cfg.BackoffFactor, "exponential backoff factor")
	flag.IntVar(&cfg.BatchSize, "batch-size", cfg.BatchSize, "number of files per batch")
	flag.IntVar(&cfg.MaxConcurrency, "max-concurrency", cfg.MaxConcurrency, "maximum concurrency for adaptive adjustment")
	flag.DurationVar(&cfg.TimeoutList, "timeout-list", cfg.TimeoutList, "timeout for file listing")
	flag.DurationVar(&cfg.TimeoutCopy, "timeout-copy", cfg.TimeoutCopy, "timeout for file copying")
	flag.DurationVar(&cfg.TimeoutCleanup, "timeout-cleanup", cfg.TimeoutCleanup, "timeout for cleanup")
	flag.Parse()
	if cfg.MaxConcurrency < cfg.Concurrency {
		cfg.MaxConcurrency = cfg.Concurrency
	}
	return cfg
}

// AdbCommand represents an ADB command with arguments and timeout.
type AdbCommand struct {
	Args    []string
	Timeout time.Duration
	Key     string
	Type    string
	Path    string
}

// run executes an ADB command with retries and exponential backoff.
func (cfg *Config) run(cmd AdbCommand, cache *Cache) (string, string, error) {
	var lastErr error
	var stdoutBuf, stderrBuf bytes.Buffer
	for attempt := 1; attempt <= cfg.MaxRetries; attempt++ {
		if cache != nil && cache.ShouldSkip(cmd.Key) {
			return stdoutBuf.String(), stderrBuf.String(), fmt.Errorf("skipped %s after %d retries", cmd.Key, cfg.MaxRetries)
		}

		ctx, cancel := context.WithTimeout(context.Background(), cmd.Timeout)
		defer cancel()
		adbCmd := exec.CommandContext(ctx, cfg.AdbPath, cmd.Args...)
		adbCmd.Stdout = &stdoutBuf
		adbCmd.Stderr = &stderrBuf
		err := adbCmd.Run()
		if err == nil {
			return stdoutBuf.String(), stderrBuf.String(), nil
		}

		lastErr = fmt.Errorf("ADB command %v failed: %w", cmd.Args, err)
		stderrStr := stderrBuf.String()
		if cache != nil {
			if strings.Contains(strings.ToLower(stderrStr), "permission denied") {
				cache.AddFileStatus(FileStatus{Path: cmd.Path, Error: true})
				return stdoutBuf.String(), stderrStr, lastErr
			}
			cache.AddFailed(cmd.Key, cmd.Type, attempt, lastErr, stderrStr)
		}
		fmt.Fprintf(os.Stderr, "Attempt %d/%d failed for %s: %v, Stderr: %s\n",
			attempt, cfg.MaxRetries, cmd.Key, lastErr, strings.TrimSpace(stderrStr))

		if attempt < cfg.MaxRetries {
			delay := cfg.RetryDelay * time.Duration(math.Pow(cfg.BackoffFactor, float64(attempt-1)))
			time.Sleep(delay)
		}
	}
	return stdoutBuf.String(), stderrBuf.String(), lastErr
}

// checkAdb validates the ADB executable path.
func (cfg *Config) checkAdb() error {
	if _, err := os.Stat(cfg.AdbPath); os.IsNotExist(err) {
		return fmt.Errorf("ADB executable not found at %s", cfg.AdbPath)
	}
	return nil
}

// checkDeviceConnectivity verifies if a device is connected.
func (cfg *Config) checkDeviceConnectivity(cache *Cache) error {
	_, _, err := cfg.run(AdbCommand{
		Args:    []string{"devices"},
		Timeout: cfg.TimeoutList,
		Key:     "check-devices",
		Type:    "Command",
		Path:    "",
	}, cache)
	return err
}

// startAdbServer starts the ADB server with retries.
func (cfg *Config) startAdbServer(cache *Cache) error {
	_, _, err := cfg.run(AdbCommand{
		Args:    []string{"start-server"},
		Timeout: cfg.TimeoutList,
		Key:     "start-server",
		Type:    "Command",
	}, cache)
	return err
}

// waitForDevice waits for an ADB device to be connected.
func (cfg *Config) waitForDevice(cache *Cache) error {
	_, _, err := cfg.run(AdbCommand{
		Args:    []string{"wait-for-device"},
		Timeout: cfg.TimeoutList,
		Key:     "wait-for-device",
		Type:    "Command",
	}, cache)
	return err
}

// backupDevice creates a backup of the Android device.
func (cfg *Config) backupDevice(cache *Cache) error {
	if err := os.MkdirAll(cfg.BackupDir, 0o755); err != nil {
		return fmt.Errorf("failed to create backup directory: %w", err)
	}
	backupFile := filepath.Join(cfg.BackupDir, fmt.Sprintf("backup_%s.ab", time.Now().Format("20060102_150405")))
	fmt.Printf("Backing up to %s\n", backupFile)
	_, _, err := cfg.run(AdbCommand{
		Args:    []string{"backup", "-f", backupFile, "-all", "-apk", "-shared"},
		Timeout: cfg.TimeoutCopy,
		Key:     backupFile,
		Type:    "File",
		Path:    backupFile,
	}, cache)
	return err
}

// listFiles lists files in a directory on the device, using parallel listing for subdirectories.
func (cfg *Config) listFiles(src string, cache *Cache) ([]string, error) {
	if cache.IsPathInSkipList(src) {
		fmt.Fprintf(os.Stderr, "Warning: skipping path %s due to previous permission denied\n", src)
		return []string{}, nil
	}

	// List subdirectories for parallel processing
	ctx, cancel := context.WithTimeout(context.Background(), cfg.TimeoutList)
	defer cancel()
	cmd := exec.CommandContext(ctx, cfg.AdbPath, "shell", "find", src, "-maxdepth", "1", "-type", "d")
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	err := cmd.Run()
	if err != nil {
		cache.AddFailed("list-dirs-"+src, "Command", 1, err, stderrBuf.String())
		fmt.Fprintf(os.Stderr, "Warning: failed to list directories in %s: %v, stderr: %s\n", src, err, stderrBuf.String())
		return cfg.listFilesSingleCtx(ctx, src, cache)
	}

	// Parse subdirectories
	var subdirs []string
	scanner := bufio.NewScanner(strings.NewReader(stdoutBuf.String()))
	for scanner.Scan() {
		dir := strings.TrimSpace(scanner.Text())
		if dir != "" && dir != src && !cache.IsPathInSkipList(dir) {
			subdirs = append(subdirs, dir)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("failed to scan subdirectories: %w", err)
	}

	// Process subdirectories in parallel
	var wg sync.WaitGroup
	var mu sync.Mutex
	var allFiles []string
	var errs []error
	sem := make(chan struct{}, cfg.Concurrency)
	for _, subdir := range append(subdirs, src) {
		sem <- struct{}{}
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			defer func() { <-sem }()
			files, err := cfg.listFilesSingleCtx(ctx, path, cache)
			mu.Lock()
			if err != nil {
				errs = append(errs, fmt.Errorf("failed to list %s: %w", path, err))
			}
			allFiles = append(allFiles, files...)
			mu.Unlock()
		}(subdir)
	}
	wg.Wait()

	if len(errs) > 0 {
		return allFiles, errors.Join(errs...)
	}
	return allFiles, nil
}

// listFilesSingleCtx lists files in a single directory with context.
func (cfg *Config) listFilesSingleCtx(ctx context.Context, src string, cache *Cache) ([]string, error) {
	cmd := exec.CommandContext(ctx, cfg.AdbPath, "shell", "find", src, "-type", "f")
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	err := cmd.Run()

	// Parse stderr for permission-denied paths
	deniedPaths := parsePermissionDeniedPaths(stderrBuf.String())
	for _, path := range deniedPaths {
		cache.AddFileStatus(FileStatus{Path: path, Error: true})
		fmt.Fprintf(os.Stderr, "Warning: permission denied for %s, marked as unprocessable\n", path)
	}

	// Cache all files
	var files []string
	scanner := bufio.NewScanner(strings.NewReader(stdoutBuf.String()))
	for scanner.Scan() {
		file := strings.TrimSpace(scanner.Text())
		if file != "" {
			hasError := cache.IsPathInSkipList(file)
			cache.AddFileStatus(FileStatus{Path: file, Error: hasError})
			if !hasError {
				files = append(files, file)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return files, fmt.Errorf("failed to scan files: %w", err)
	}

	if err != nil {
		cache.AddFailed("list-files-"+src, "Command", 1, err, stderrBuf.String())
		fmt.Fprintf(os.Stderr, "Warning: errors encountered listing files in %s: %v, stderr: %s\n", src, err, stderrBuf.String())
		return files, nil
	}
	return files, nil
}

// parsePermissionDeniedPaths extracts permission-denied paths from stderr.
func parsePermissionDeniedPaths(stderr string) []string {
	var paths []string
	re := regexp.MustCompile(`find:\s*(.*?):\s*Permission denied`)
	matches := re.FindAllStringSubmatch(stderr, -1)
	for _, match := range matches {
		if len(match) > 1 {
			path := strings.TrimSpace(match[1])
			if path != "" {
				paths = append(paths, path)
			}
		}
	}
	return paths
}

// SanitizePath removes dangerous characters and validates paths.
func SanitizePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if filepath.IsAbs(path) {
		return "", fmt.Errorf("absolute paths not allowed: %s", path)
	}
	cleaned := filepath.Clean(path)
	if strings.ContainsAny(cleaned, "<>:|?*") {
		return "", fmt.Errorf("invalid characters in path: %s", path)
	}
	return cleaned, nil
}

// copyFilesBatch copies a batch of files using compressed tar.
func (cfg *Config) copyFilesBatch(files []string, dst, rootSrc string, cache *Cache, wg *sync.WaitGroup, sem chan struct{}) error {
	defer wg.Done()
	defer func() { <-sem }()

	if len(files) == 0 {
		return nil
	}

	// Create temporary tar file on device
	tmpTarBase := fmt.Sprintf("backup_%d.tar.gz", time.Now().UnixNano())
	tmpTar := filepath.Join("/data/local/tmp", tmpTarBase)
	if _, err := SanitizePath(tmpTar); err != nil {
		return fmt.Errorf("invalid temporary tar path: %w", err)
	}

	var cleanupErr error
	defer func() {
		_, _, err := cfg.run(AdbCommand{
			Args:    []string{"shell", "rm", "-f", tmpTar},
			Timeout: cfg.TimeoutCleanup,
			Key:     "cleanup-" + tmpTar,
			Type:    "Command",
			Path:    tmpTar,
		}, cache)
		if err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("failed to cleanup temporary tar file %s: %w", tmpTar, err))
			fmt.Fprintf(os.Stderr, "Warning: failed to cleanup %s: %v\n", tmpTar, err)
		}
	}()

	// Create compressed tar archive with relative paths
	var relativeFiles []string
	for _, file := range files {
		relPath, err := filepath.Rel(rootSrc, file)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to relativize %s: %v, skipping\n", file, err)
			continue
		}
		relativeFiles = append(relativeFiles, relPath)
	}
	fileList := strings.Join(relativeFiles, "\n")
	ctx, cancel := context.WithTimeout(context.Background(), cfg.TimeoutCopy)
	defer cancel()
	tarCmd := exec.CommandContext(ctx, cfg.AdbPath, "shell", "tar", "-zcf", tmpTar, "-T", "-")
	tarCmd.Stdin = strings.NewReader(fileList)
	var stderrBuf bytes.Buffer
	tarCmd.Stderr = &stderrBuf
	err := tarCmd.Run()
	if err != nil {
		stderr := stderrBuf.String()
		if strings.Contains(strings.ToLower(stderr), "permission denied") {
			for _, file := range files {
				cache.AddFileStatus(FileStatus{Path: file, Error: true})
				fmt.Fprintf(os.Stderr, "Warning: permission denied for %s, marked as unprocessable\n", file)
			}
		}
		return errors.Join(fmt.Errorf("failed to create tar archive: %w, stderr: %s", err, stderr), cleanupErr)
	}

	// Pull tar archive
	localTar := filepath.Join(dst, fmt.Sprintf("batch_%d.tar.gz", time.Now().UnixNano()))
	_, stderr, err := cfg.run(AdbCommand{
		Args:    []string{"pull", tmpTar, localTar},
		Timeout: cfg.TimeoutCopy,
		Key:     "pull-" + tmpTar,
		Type:    "File",
		Path:    tmpTar,
	}, cache)
	if err != nil {
		return errors.Join(fmt.Errorf("failed to pull tar archive %s: %w, stderr: %s", tmpTar, err, stderr), cleanupErr)
	}
	defer os.Remove(localTar)

	// Extract tar archive
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return errors.Join(fmt.Errorf("failed to create destination directory %s: %w", dst, err), cleanupErr)
	}
	untarCmd := exec.Command("tar", "-zxf", localTar, "-C", dst)
	if err := untarCmd.Run(); err != nil {
		return errors.Join(fmt.Errorf("failed to extract tar archive %s: %w", localTar, err), cleanupErr)
	}

	// Log successful copies
	for _, file := range files {
		relativePath, err := filepath.Rel(rootSrc, file)
		dstPath := filepath.Join(dst, relativePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to relativize %s: %v, using basename\n", file, err)
			dstPath = filepath.Join(dst, filepath.Base(file))
		}
		fmt.Printf("Copied %s to %s\n", file, dstPath)
	}

	return cleanupErr
}

// copyFromDevice copies files with adaptive concurrency.
func (cfg *Config) copyFromDevice(cache *Cache) error {
	const defaultSrc = "/storage/emulated/0/"
	alternativePaths := []string{"/sdcard/", "/mnt/sdcard/"}
	attemptedPaths := make(map[string]bool)

	for {
		src := prompt(fmt.Sprintf("Enter source path on device [%s]: ", defaultSrc))
		if src == "" {
			src = defaultSrc
		}
		rootSrc := strings.TrimRight(src, "/")

		if err := cfg.checkDeviceConnectivity(cache); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: device connectivity check failed: %v\n", err)
		}

		if attemptedPaths[src] {
			fmt.Fprintf(os.Stderr, "Warning: path %s already attempted, please try a different path\n", src)
			continue
		}
		attemptedPaths[src] = true

		_, stderr, err := cfg.run(AdbCommand{
			Args:    []string{"shell", "ls", src},
			Timeout: cfg.TimeoutList,
			Key:     "validate-" + src,
			Type:    "Command",
			Path:    src,
		}, cache)
		if err != nil {
			if strings.Contains(strings.ToLower(stderr), "permission denied") {
				cache.AddFileStatus(FileStatus{Path: src, Error: true})
				fmt.Fprintf(os.Stderr, "Warning: permission denied for %s, marked as unprocessable\n", src)
			}
			fmt.Fprintf(os.Stderr, "Warning: invalid source path %s: %v\n", src, err)
			for _, altSrc := range alternativePaths {
				if !attemptedPaths[altSrc] {
					fmt.Printf("Trying alternative path: %s\n", altSrc)
					src = altSrc
					rootSrc = strings.TrimRight(altSrc, "/")
					attemptedPaths[altSrc] = true
					err = nil
					break
				}
			}
			if err != nil {
				choice := prompt("Enter a new source path or press Enter to return to main menu: ")
				if choice == "" {
					return nil
				}
				src = choice
				rootSrc = strings.TrimRight(src, "/")
				continue
			}
		}

		dst := filepath.Join(cfg.BackupDir, "files", time.Now().Format("20060102_150405"))
		files, err := cfg.listFiles(src, cache)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to list files in %s: %v\n", src, err)
			choice := prompt("Enter a new source path or press Enter to return to main menu: ")
			if choice == "" {
				return nil
			}
			src = choice
			rootSrc = strings.TrimRight(src, "/")
			continue
		}

		// Adaptive concurrency
		concurrency := cfg.Concurrency
		var wg sync.WaitGroup
		sem := make(chan struct{}, cfg.MaxConcurrency)
		var mu sync.Mutex
		avgDuration := 0.0
		count := 0
		for i := 0; i < len(files); i += cfg.BatchSize {
			end := i + cfg.BatchSize
			if end > len(files) {
				end = len(files)
			}
			batch := files[i:end]
			wg.Add(1)
			go func(b []string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				start := time.Now()
				if err := cfg.copyFilesBatch(b, dst, rootSrc, cache, &wg, sem); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: batch copy failed: %v\n", err)
				}
				mu.Lock()
				avgDuration = (avgDuration*float64(count) + float64(time.Since(start))) / float64(count+1)
				count++
				if avgDuration > float64(2*time.Second) && concurrency > 1 {
					concurrency--
				} else if avgDuration < float64(500*time.Millisecond) && concurrency < cfg.MaxConcurrency {
					concurrency++
				}
				mu.Unlock()
			}(batch)
		}
		wg.Wait()

		// Retry failed files
		failed := cache.GetFailed()
		if len(failed) > 0 {
			fmt.Printf("Retrying %d failed files\n", len(failed))
			for i := 0; i < len(failed); i += cfg.BatchSize {
				end := i + cfg.BatchSize
				if end > len(failed) {
					end = len(failed)
				}
				batch := failed[i:end]
				wg.Add(1)
				go func(b []string) {
					defer wg.Done()
					sem <- struct{}{}
					defer func() { <-sem }()
					if err := cfg.copyFilesBatch(b, dst, rootSrc, cache, &wg, sem); err != nil {
						fmt.Fprintf(os.Stderr, "Warning: batch copy failed: %v\n", err)
					}
				}(batch)
			}
			wg.Wait()
		}

		return nil
	}
}

// restoreDevice restores a backup to the Android device.
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
	_, _, err = cfg.run(AdbCommand{
		Args:    []string{"restore", backupFile},
		Timeout: cfg.TimeoutCopy,
		Key:     backupFile,
		Type:    "File",
		Path:    backupFile,
	}, cache)
	return err
}

// prompt reads a trimmed string from stdin with path sanitization.
func prompt(msg string) string {
	fmt.Print(msg)
	scanner := bufio.NewScanner(os.Stdin)
	if scanner.Scan() {
		input := strings.TrimSpace(scanner.Text())
		if input != "" {
			if sanitized, err := SanitizePath(input); err == nil {
				return sanitized
			}
			fmt.Fprintf(os.Stderr, "Invalid path: %s\n", input)
		}
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

// main updates error handling for Windows
func main() {
	cfg := NewConfig()
	cache, err := NewCache(cfg.LogFile, cfg.SkipListFile, cfg.MaxRetries)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Fatal error: %v\nOn Windows, try:\n1. Right-click %s > Properties > Security, ensure your user has Read/Write permissions\n2. Run the program as administrator\n3. Ensure no other program is using %s\n", err, cfg.SkipListFile, cfg.SkipListFile)
		os.Exit(1)
	}
	defer cache.Close()

	if err := cfg.checkAdb(); err != nil {
		fmt.Fprintf(os.Stderr, "Fatal error: %v\nEnsure ADB is installed and accessible in your PATH\n", err)
		os.Exit(1)
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
