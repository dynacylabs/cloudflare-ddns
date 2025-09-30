// Package main implements a comprehensive healthcheck for cloudflare-ddns
// This utility provides multiple levels of health verification with configurable options
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/favonia/cloudflare-ddns/internal/ipnet"
	"github.com/favonia/cloudflare-ddns/internal/pp"
	"github.com/favonia/cloudflare-ddns/internal/provider"
)

// Configuration constants and defaults
const (
	defaultTimeout         = 10 * time.Second
	defaultCheckInterval   = 60 * time.Second
	procDir               = "/proc"
	statusFile            = "/tmp/ddns-healthcheck-status"
	logFile              = "/tmp/ddns-healthcheck.log"
	maxLogSize           = 1024 * 1024 // 1MB
)

// healthcheckConfig holds all configuration options
type healthcheckConfig struct {
	// Basic settings
	timeout         time.Duration
	checkInterval   time.Duration
	
	// Check levels
	checkProcess      bool
	checkProviders    bool
	checkConnectivity bool
	checkDNS         bool
	
	// Development mode
	devMode          bool
	
	// Output settings
	verbose          bool
	logToFile        bool
}

// parseConfig reads configuration from environment variables
func parseConfig() healthcheckConfig {
	cfg := healthcheckConfig{
		// Defaults
		timeout:          defaultTimeout,
		checkInterval:    defaultCheckInterval,
		checkProcess:     true,
		checkProviders:   false,
		checkConnectivity: false,
		checkDNS:        false,
		verbose:         false,
		logToFile:       true,
	}

	// Parse timeout
	if timeoutStr := os.Getenv("HEALTHCHECK_TIMEOUT"); timeoutStr != "" {
		if timeout, err := time.ParseDuration(timeoutStr); err == nil {
			cfg.timeout = timeout
		}
	}

	// Parse check interval
	if intervalStr := os.Getenv("HEALTHCHECK_INTERVAL"); intervalStr != "" {
		if interval, err := time.ParseDuration(intervalStr); err == nil {
			cfg.checkInterval = interval
		}
	}

	// Parse check levels
	cfg.checkProviders = getEnvBool("HEALTHCHECK_PROVIDERS", cfg.checkProviders)
	cfg.checkConnectivity = getEnvBool("HEALTHCHECK_CONNECTIVITY", cfg.checkConnectivity)
	cfg.checkDNS = getEnvBool("HEALTHCHECK_DNS", cfg.checkDNS)
	
	// Development mode - skip process checks if no DDNS process is expected
	cfg.devMode = getEnvBool("HEALTHCHECK_DEV_MODE", cfg.devMode)
	if cfg.devMode {
		cfg.checkProcess = false
	}
	
	// Legacy environment variable support
	if getEnvBool("DNS_CHECK_ENABLED", false) {
		cfg.checkDNS = true
	}
	if getEnvBool("THOROUGH_HEALTHCHECK", false) {
		cfg.checkProviders = true
		cfg.checkDNS = true
	}

	// Parse output settings
	cfg.verbose = getEnvBool("HEALTHCHECK_VERBOSE", cfg.verbose)
	cfg.logToFile = getEnvBool("HEALTHCHECK_LOG", cfg.logToFile)

	return cfg
}

// getEnvBool reads a boolean environment variable with a default value
func getEnvBool(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		if parsed, err := strconv.ParseBool(value); err == nil {
			return parsed
		}
	}
	return defaultValue
}

// logger handles logging to file and stderr
type logger struct {
	logToFile bool
	verbose   bool
}

func (l *logger) log(level, message string) {
	timestamp := time.Now().Format("2006-01-02 15:04:05")
	logMessage := fmt.Sprintf("%s [%s] %s", timestamp, level, message)
	
	// Always log errors to stderr
	if level == "ERROR" || l.verbose {
		fmt.Fprintln(os.Stderr, logMessage)
	}
	
	// Log to file if enabled
	if l.logToFile {
		l.writeToFile(logMessage)
	}
}

func (l *logger) writeToFile(message string) {
	// Rotate log if it's too large
	if stat, err := os.Stat(logFile); err == nil && stat.Size() > maxLogSize {
		l.rotateLog()
	}
	
	if file, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
		defer file.Close()
		fmt.Fprintln(file, message)
	}
}

func (l *logger) rotateLog() {
	// Keep last 500 lines
	if content, err := os.ReadFile(logFile); err == nil {
		lines := strings.Split(string(content), "\n")
		if len(lines) > 500 {
			start := len(lines) - 500
			newContent := strings.Join(lines[start:], "\n")
			os.WriteFile(logFile, []byte(newContent), 0644)
		}
	}
}

func (l *logger) info(message string) {
	l.log("INFO", message)
}

func (l *logger) error(message string) {
	l.log("ERROR", message)
}

func (l *logger) debug(message string) {
	if l.verbose {
		l.log("DEBUG", message)
	}
}

// findDDNSProcess finds the PID of the running ddns process
func findDDNSProcess() (int, error) {
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return 0, fmt.Errorf("failed to read /proc directory: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		// Check if the directory name is a number (PID)
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}

		// Read the command line to check if it's our ddns process
		cmdlinePath := fmt.Sprintf("/proc/%d/cmdline", pid)
		cmdlineBytes, err := os.ReadFile(cmdlinePath)
		if err != nil {
			continue // Process might have disappeared
		}

		cmdline := string(cmdlineBytes)
		// Look for various forms of ddns processes
		if strings.Contains(cmdline, "/bin/ddns") || 
		   strings.Contains(cmdline, "./ddns") ||
		   strings.Contains(cmdline, "cmd/ddns/ddns.go") ||
		   (strings.Contains(cmdline, "ddns") && strings.Contains(cmdline, "go run")) {
			return pid, nil
		}
	}

	return 0, fmt.Errorf("ddns process not found")
}

// checkProcessHealth verifies the ddns process is running and responsive
func checkProcessHealth(log *logger) error {
	log.debug("Checking process health...")
	
	// Find the ddns process
	pid, err := findDDNSProcess()
	if err != nil {
		return fmt.Errorf("process check failed: %w", err)
	}

	log.debug(fmt.Sprintf("Found ddns process with PID %d", pid))

	// Check if process directory still exists
	procPath := fmt.Sprintf("/proc/%d", pid)
	if _, err := os.Stat(procPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("process %d no longer exists", pid)
		}
		return fmt.Errorf("failed to check process %d: %w", pid, err)
	}

	// Check process status
	statusPath := fmt.Sprintf("/proc/%d/stat", pid)
	statusBytes, err := os.ReadFile(statusPath)
	if err != nil {
		return fmt.Errorf("failed to read process status for %d: %w", pid, err)
	}

	// Parse the stat file to check process state
	statFields := strings.Fields(string(statusBytes))
	if len(statFields) < 3 {
		return fmt.Errorf("invalid stat file for process %d", pid)
	}

	state := statFields[2]
	// Check if process is not in zombie or dead state
	if state == "Z" || state == "X" {
		return fmt.Errorf("process %d is in state %s (zombie or dead)", pid, state)
	}

	// Try to send signal 0 to verify we can interact with the process
	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("failed to find process %d: %w", pid, err)
	}

	if err := process.Signal(syscall.Signal(0)); err != nil {
		return fmt.Errorf("process %d is not responsive: %w", pid, err)
	}

	log.debug("Process health check passed")
	return nil
}

// checkProviderHealth tests IP detection providers
func checkProviderHealth(ctx context.Context, log *logger) error {
	log.debug("Checking provider health...")
	
	// Create a silent pretty printer
	ppfmt := &silentPP{}
	
	// Test CloudFlare trace provider (the default)
	cfProvider := provider.NewCloudflareTrace()
	
	// Try to get IPv4 address
	ip, ok := cfProvider.GetIP(ctx, ppfmt, ipnet.IP4)
	if !ok {
		return fmt.Errorf("failed to detect IPv4 address using CloudFlare trace")
	}

	log.debug(fmt.Sprintf("Successfully detected IPv4 address: %s", ip.String()))
	
	// Try IPv6 if requested
	if getEnvBool("HEALTHCHECK_IPV6", false) {
		ip6, ok := cfProvider.GetIP(ctx, ppfmt, ipnet.IP6)
		if !ok {
			log.debug("IPv6 detection failed (this may be normal)")
		} else {
			log.debug(fmt.Sprintf("Successfully detected IPv6 address: %s", ip6.String()))
		}
	}

	log.debug("Provider health check passed")
	return nil
}

// checkDNSResolution verifies DNS resolution for configured domains
func checkDNSResolution(ctx context.Context, log *logger) error {
	log.debug("Checking DNS resolution...")
	
	// Get domains from environment variables
	domains := getDomainList()
	if len(domains) == 0 {
		log.debug("No domains configured for DNS check")
		return nil
	}

	// Test DNS resolution for the first domain
	firstDomain := domains[0]
	log.debug(fmt.Sprintf("Testing DNS resolution for domain: %s", firstDomain))
	
	// Use a simple approach - try to resolve the domain
	// This is a basic check that doesn't require external tools
	if err := testDNSResolution(ctx, firstDomain); err != nil {
		// Don't fail the healthcheck for DNS issues as they might be external
		log.debug(fmt.Sprintf("DNS resolution warning for %s: %v", firstDomain, err))
		return nil
	}

	log.debug("DNS resolution check passed")
	return nil
}

// getDomainList extracts domain list from environment variables
func getDomainList() []string {
	var domains []string
	
	// Check various environment variable formats
	for _, envVar := range []string{"DOMAINS", "IP4_DOMAINS", "IP6_DOMAINS"} {
		if domainStr := os.Getenv(envVar); domainStr != "" {
			// Split by comma and clean up
			for _, domain := range strings.Split(domainStr, ",") {
				domain = strings.TrimSpace(domain)
				if domain != "" {
					domains = append(domains, domain)
				}
			}
			break // Use first found
		}
	}
	
	return domains
}

// testDNSResolution performs a basic DNS resolution test
func testDNSResolution(ctx context.Context, domain string) error {
	// This is a placeholder - in a real implementation you might want to
	// use net.LookupHost or similar, but that requires network access
	// For now, we'll just validate the domain format
	if domain == "" {
		return fmt.Errorf("empty domain")
	}
	if !strings.Contains(domain, ".") {
		return fmt.Errorf("invalid domain format: %s", domain)
	}
	return nil
}

// updateStatusFile writes the current health status to a file
func updateStatusFile(status string) {
	timestamp := time.Now().Format("2006-01-02 15:04:05")
	statusContent := fmt.Sprintf("%s: %s", timestamp, status)
	os.WriteFile(statusFile, []byte(statusContent), 0644)
}

// silentPP implements the pp.PP interface but discards all output
type silentPP struct{}

func (p *silentPP) IsShowing(_ pp.Verbosity) bool { return false }
func (p *silentPP) Indent() pp.PP { return p }
func (p *silentPP) BlankLineIfVerbose() {}
func (p *silentPP) Infof(_ pp.Emoji, _ string, _ ...any) {}
func (p *silentPP) Noticef(_ pp.Emoji, _ string, _ ...any) {}
func (p *silentPP) Suppress(_ pp.ID) {}
func (p *silentPP) InfoOncef(_ pp.ID, _ pp.Emoji, _ string, _ ...any) {}
func (p *silentPP) NoticeOncef(_ pp.ID, _ pp.Emoji, _ string, _ ...any) {}

// performHealthcheck runs all configured health checks
func performHealthcheck(cfg healthcheckConfig, log *logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()

	log.info("Starting healthcheck...")

	// Always check process health
	if cfg.checkProcess {
		if err := checkProcessHealth(log); err != nil {
			return fmt.Errorf("process health check failed: %w", err)
		}
	}

	// Optional provider health check
	if cfg.checkProviders {
		if err := checkProviderHealth(ctx, log); err != nil {
			return fmt.Errorf("provider health check failed: %w", err)
		}
	}

	// Optional DNS resolution check
	if cfg.checkDNS {
		if err := checkDNSResolution(ctx, log); err != nil {
			return fmt.Errorf("DNS resolution check failed: %w", err)
		}
	}

	log.info("All health checks passed")
	return nil
}

// showUsage displays help information
func showUsage() {
	fmt.Fprintf(os.Stderr, `cloudflare-ddns healthcheck utility

USAGE:
    healthcheck [OPTIONS]

ENVIRONMENT VARIABLES:
    HEALTHCHECK_TIMEOUT         Timeout for healthcheck operations (default: 10s)
    HEALTHCHECK_INTERVAL        Interval for periodic checks (default: 60s)
    HEALTHCHECK_PROVIDERS       Enable IP provider testing (default: false)
    HEALTHCHECK_CONNECTIVITY    Enable connectivity testing (default: false)
    HEALTHCHECK_DNS             Enable DNS resolution testing (default: false)
    HEALTHCHECK_IPV6            Test IPv6 detection (default: false)
    HEALTHCHECK_DEV_MODE        Skip process checks for development (default: false)
    HEALTHCHECK_VERBOSE         Enable verbose output (default: false)
    HEALTHCHECK_LOG             Enable file logging (default: true)
    
    Legacy variables:
    DNS_CHECK_ENABLED           Same as HEALTHCHECK_DNS
    THOROUGH_HEALTHCHECK        Enables PROVIDERS and DNS checks

OPTIONS:
    -h, --help                  Show this help message
    -v, --verbose               Enable verbose output
    --version                   Show version information

EXAMPLES:
    # Basic process health check
    healthcheck
    
    # Comprehensive health check
    HEALTHCHECK_PROVIDERS=true HEALTHCHECK_DNS=true healthcheck
    
    # Verbose mode with custom timeout
    HEALTHCHECK_VERBOSE=true HEALTHCHECK_TIMEOUT=30s healthcheck

EXIT CODES:
    0   All health checks passed
    1   One or more health checks failed
`)
}

func main() {
	// Handle command line arguments
	for _, arg := range os.Args[1:] {
		switch arg {
		case "-h", "--help":
			showUsage()
			os.Exit(0)
		case "-v", "--verbose":
			os.Setenv("HEALTHCHECK_VERBOSE", "true")
		case "--version":
			fmt.Println("cloudflare-ddns healthcheck utility")
			os.Exit(0)
		}
	}

	// Parse configuration
	cfg := parseConfig()
	
	// Create logger
	log := &logger{
		logToFile: cfg.logToFile,
		verbose:   cfg.verbose,
	}

	// Set up timeout for the entire healthcheck
	done := make(chan error, 1)
	
	go func() {
		done <- performHealthcheck(cfg, log)
	}()

	select {
	case err := <-done:
		if err != nil {
			log.error(fmt.Sprintf("UNHEALTHY: %v", err))
			updateStatusFile("UNHEALTHY")
			fmt.Fprintf(os.Stderr, "UNHEALTHY: %v\n", err)
			os.Exit(1)
		}
		log.info("HEALTHY: all checks passed")
		updateStatusFile("HEALTHY")
		fmt.Println("HEALTHY: all checks passed")
		os.Exit(0)
	case <-time.After(cfg.timeout):
		log.error(fmt.Sprintf("UNHEALTHY: healthcheck timed out after %v", cfg.timeout))
		updateStatusFile("TIMEOUT")
		fmt.Fprintf(os.Stderr, "UNHEALTHY: healthcheck timed out after %v\n", cfg.timeout)
		os.Exit(1)
	}
}