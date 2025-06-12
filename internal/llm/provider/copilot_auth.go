package provider

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// CopilotAuth handles GitHub Copilot authentication
type CopilotAuth struct{}

// NewCopilotAuth creates a new CopilotAuth instance
func NewCopilotAuth() *CopilotAuth {
	return &CopilotAuth{}
}

// GetOAuthToken retrieves the GitHub OAuth token from Copilot configuration
func (c *CopilotAuth) GetOAuthToken() (string, error) {
	// Try multiple paths for GitHub Copilot config
	configPaths := c.getConfigPaths()

	for _, path := range configPaths {
		if token := c.tryReadTokenFromPath(path); token != "" {
			return token, nil
		}
	}

	// Try to get token from GitHub CLI
	if token := c.tryGetTokenFromGitHubCLI(); token != "" {
		return token, nil
	}

	return "", fmt.Errorf("no GitHub OAuth token found. Please authenticate with GitHub Copilot or GitHub CLI first")
}

// IsAuthenticated checks if the user has a valid OAuth token
func (c *CopilotAuth) IsAuthenticated() bool {
	token, _ := c.GetOAuthToken()
	return token != ""
}

// GetAuthenticationInstructions returns instructions for authentication
func (c *CopilotAuth) GetAuthenticationInstructions() string {
	return `To use GitHub Copilot, you need to authenticate first. Please choose one of these options:

1. Install and authenticate with GitHub CLI:
   - Install GitHub CLI: https://cli.github.com/
   - Run: gh auth login
   - Run: gh copilot auth

2. Use GitHub Copilot in VS Code or another editor first:
   - Install GitHub Copilot extension
   - Sign in when prompted
   - This will create the necessary authentication files

3. Manual authentication:
   - Visit: https://github.com/login/device
   - Follow the device flow instructions

After authentication, restart OpenCode to use Copilot models.`
}

func (c *CopilotAuth) getConfigPaths() []string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return []string{}
	}

	var paths []string

	switch runtime.GOOS {
	case "windows":
		// Windows paths
		paths = []string{
			filepath.Join(homeDir, "AppData", "Local", "github-copilot", "hosts.json"),
			filepath.Join(homeDir, "AppData", "Local", "github-copilot", "apps.json"),
			filepath.Join(homeDir, ".config", "github-copilot", "hosts.json"),
			filepath.Join(homeDir, ".config", "github-copilot", "apps.json"),
		}
	case "darwin":
		// macOS paths
		paths = []string{
			filepath.Join(homeDir, ".config", "github-copilot", "hosts.json"),
			filepath.Join(homeDir, ".config", "github-copilot", "apps.json"),
			filepath.Join(homeDir, "Library", "Application Support", "github-copilot", "hosts.json"),
			filepath.Join(homeDir, "Library", "Application Support", "github-copilot", "apps.json"),
		}
	default:
		// Linux and other Unix-like systems
		paths = []string{
			filepath.Join(homeDir, ".config", "github-copilot", "hosts.json"),
			filepath.Join(homeDir, ".config", "github-copilot", "apps.json"),
		}
	}

	return paths
}

func (c *CopilotAuth) tryReadTokenFromPath(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}

	var config map[string]interface{}
	if err := json.Unmarshal(data, &config); err != nil {
		return ""
	}

	// Look for GitHub.com entries
	for key, value := range config {
		if strings.Contains(key, "github.com") {
			if hostConfig, ok := value.(map[string]interface{}); ok {
				if token, ok := hostConfig["oauth_token"].(string); ok && token != "" {
					return token
				}
			}
		}
	}

	return ""
}

func (c *CopilotAuth) tryGetTokenFromGitHubCLI() string {
	// Try to get token from GitHub CLI
	cmd := exec.Command("gh", "auth", "token")
	output, err := cmd.Output()
	if err != nil {
		return ""
	}

	token := strings.TrimSpace(string(output))
	if token != "" && strings.HasPrefix(token, "ghp_") {
		return token
	}

	return ""
}

// CopilotConfigPaths returns the paths where Copilot stores configuration
func (c *CopilotAuth) CopilotConfigPaths() []string {
	return c.getConfigPaths()
}
