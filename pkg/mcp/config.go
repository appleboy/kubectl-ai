// Copyright 2025 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mcp

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"k8s.io/klog/v2"
	"sigs.k8s.io/yaml"
)

// ServerConfig represents the configuration for an MCP server
type ServerConfig struct {
	// Name is a friendly name for this MCP server
	Name string `yaml:"name"`
	// Command is the command to execute for stdio-based MCP servers
	Command string `yaml:"command"`
	// Args are the arguments to pass to the command
	Args []string `yaml:"args,omitempty"`
	// Env are the environment variables to set for the command
	Env map[string]string `yaml:"env,omitempty"`
}

// Config represents the MCP client configuration file
type Config struct {
	// Servers is a list of MCP server configurations
	Servers []ServerConfig `yaml:"servers,omitempty"`
}

// loadDefaultConfig loads the default configuration from the embedded file
func loadDefaultConfig() (*Config, error) {
	// This path is relative to the module root
	defaultConfigPath := filepath.Join("pkg", "mcp", "default_config.yaml")

	// Read the file
	data, err := os.ReadFile(defaultConfigPath)
	if err != nil {
		return nil, fmt.Errorf("reading default config file: %w", err)
	}

	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parsing default config: %w", err)
	}

	return &config, nil
}

// DefaultConfigPath returns the default path to the MCP config file
func DefaultConfigPath() (string, error) {
	// Get the home directory first
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("getting user home directory: %w", err)
	}

	var configPath string

	// Handle different operating systems
	switch runtime.GOOS {
	case "windows":
		// On Windows, use %APPDATA%\kubectl-ai\mcp.yaml
		appData := os.Getenv("APPDATA")
		if appData == "" {
			appData = filepath.Join(home, "AppData", "Roaming")
		}
		configPath = filepath.Join(appData, "kubectl-ai", "mcp.yaml")
	default:
		// On Unix-like systems, use XDG_CONFIG_HOME/kubectl-ai/mcp.yaml
		configDir := os.Getenv("XDG_CONFIG_HOME")
		if configDir == "" {
			configDir = filepath.Join(home, ".config")
		}
		configPath = filepath.Join(configDir, "kubectl-ai", "mcp.yaml")
	}

	return configPath, nil
}

// LoadConfig loads the MCP configuration from the given path
func LoadConfig(path string) (*Config, error) {
	if path == "" {
		var err error
		path, err = DefaultConfigPath()
		if err != nil {
			return nil, err
		}
	}

	// If the file doesn't exist, create it with default configuration
	if _, err := os.Stat(path); os.IsNotExist(err) {
		// Create the directory if it doesn't exist
		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, ConfigDirPermissions); err != nil {
			return nil, fmt.Errorf("creating config directory: %w", err)
		}

		// Read the default config from the embedded file
		defaultConfig, err := loadDefaultConfig()
		if err != nil {
			return nil, fmt.Errorf("loading default config: %w", err)
		}

		// Save the default configuration to the user's config path
		if err := defaultConfig.Save(path); err != nil {
			return nil, fmt.Errorf("saving default config: %w", err)
		}

		klog.V(2).Info("Created default MCP configuration", "path", path)
		return defaultConfig, nil
	}

	// Load existing configuration
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parsing YAML config file: %w", err)
	}

	// Validate the configuration
	if err := config.ValidateConfig(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return &config, nil
}

// Save saves the configuration to the given path using atomic write
func (c *Config) Save(path string) error {
	if path == "" {
		var err error
		path, err = DefaultConfigPath()
		if err != nil {
			return err
		}
	}

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(path), ConfigDirPermissions); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	// Marshal the config to YAML
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	// Perform atomic write
	if err := atomicWriteFile(path, data, ConfigFilePermissions); err != nil {
		return fmt.Errorf("writing config file: %w", err)
	}

	klog.V(2).Info("Saved MCP configuration", "path", path)
	return nil
}

// atomicWriteFile writes data to a file atomically using a temporary file
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)

	// Create temporary file in the same directory
	tmpFile, err := os.CreateTemp(dir, ".mcp-config-*")
	if err != nil {
		return fmt.Errorf("creating temporary file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath) // Clean up on error

	// Write data to temporary file
	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		return fmt.Errorf("writing to temporary file: %w", err)
	}

	// Sync and close
	if err := tmpFile.Sync(); err != nil {
		tmpFile.Close()
		return fmt.Errorf("syncing temporary file: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("closing temporary file: %w", err)
	}

	// Set permissions
	if err := os.Chmod(tmpPath, perm); err != nil {
		return fmt.Errorf("setting file permissions: %w", err)
	}

	// Atomic rename
	return os.Rename(tmpPath, path)
}

// ValidateConfig validates the entire configuration
func (c *Config) ValidateConfig() error {
	if len(c.Servers) == 0 {
		return fmt.Errorf("no servers configured")
	}

	// Check for duplicate server names
	serverNames := make(map[string]bool)
	for i, server := range c.Servers {
		if err := ValidateServerConfig(server); err != nil {
			return fmt.Errorf("server %d (%s): %w", i, server.Name, err)
		}

		if serverNames[server.Name] {
			return fmt.Errorf("duplicate server name: %s", server.Name)
		}
		serverNames[server.Name] = true
	}

	return nil
}

// ValidateServerConfig validates a single server configuration
func ValidateServerConfig(config ServerConfig) error {
	if config.Name == "" {
		return fmt.Errorf("server name cannot be empty")
	}

	if config.Command == "" {
		return fmt.Errorf("server command cannot be empty")
	}

	// Additional validation could be added here:
	// - Check if command exists and is executable
	// - Validate environment variable format
	// - Check argument validity

	return nil
}
