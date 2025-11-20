package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/huh"
)

// ============================================================================
// STRUCTS AND TYPES
// ============================================================================

type Config struct {
	ProjectPath string              `json:"project_path"`
	DeployPath  string              `json:"deploy_path"`
	Modules     map[string]Module   `json:"modules"`
	Profiles    []Profile           `json:"profiles"`
}

type Module struct {
	BasePath     string `json:"base_path"`
	ArtifactFile string `json:"artifact_file"`
}

type Profile struct {
	Name        string   `json:"name"`
	ProjectPath string   `json:"project_path"`
	DeployPath  string   `json:"deploy_path"`
	Modules     []string `json:"modules"`
}

type Action string

const (
	ActionCopyOnly        Action = "Copy Only"
	ActionCompileOnly     Action = "Compile Only"
	ActionCompileAndCopy  Action = "Compile and Copy"
)

// ============================================================================
// CONFIGURATION FUNCTIONS
// ============================================================================

func loadConfig(configPath string) (*Config, error) {
	file, err := os.Open(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open config file: %w", err)
	}
	defer file.Close()

	bytes, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var config Config
	if err := json.Unmarshal(bytes, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config JSON: %w", err)
	}

	return &config, nil
}

func resolveProfileModules(profile Profile, config *Config, processedProfiles map[string]bool) ([]string, error) {
	if processedProfiles[profile.Name] {
		return nil, fmt.Errorf("circular profile reference detected: %s", profile.Name)
	}
	processedProfiles[profile.Name] = true

	var resolvedModules []string

	for _, moduleName := range profile.Modules {
		// Check if it's a profile reference ($PROFILE_NAME$)
		if strings.HasPrefix(moduleName, "$") && strings.HasSuffix(moduleName, "$") {
			referencedProfileName := strings.Trim(moduleName, "$")

			// Find the referenced profile
			var referencedProfile *Profile
			for _, p := range config.Profiles {
				if p.Name == referencedProfileName {
					referencedProfile = &p
					break
				}
			}

			if referencedProfile == nil {
				return nil, fmt.Errorf("profile reference not found: %s", referencedProfileName)
			}

			// Recursively resolve the referenced profile
			referencedModules, err := resolveProfileModules(*referencedProfile, config, processedProfiles)
			if err != nil {
				return nil, err
			}

			resolvedModules = append(resolvedModules, referencedModules...)
		} else {
			// It's a regular module
			if _, exists := config.Modules[moduleName]; !exists {
				return nil, fmt.Errorf("module not found in config: %s", moduleName)
			}
			resolvedModules = append(resolvedModules, moduleName)
		}
	}

	return resolvedModules, nil
}

func getEffectiveProjectPath(profile Profile, config *Config) string {
	if profile.ProjectPath != "" {
		return profile.ProjectPath
	}
	return config.ProjectPath
}

func getEffectiveDeployPath(profile Profile, config *Config) string {
	if profile.DeployPath != "" {
		return profile.DeployPath
	}
	return config.DeployPath
}

// ============================================================================
// UX FUNCTIONS (HUH INTERACTIVE FORMS)
// ============================================================================

func selectProfile(profiles []Profile) (*Profile, error) {
	if len(profiles) == 0 {
		return nil, fmt.Errorf("no profiles configured")
	}

	var selectedProfileName string
	options := make([]huh.Option[string], len(profiles))

	for i, profile := range profiles {
		options[i] = huh.NewOption(profile.Name, profile.Name)
	}

	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Select a profile:").
				Options(options...).
				Value(&selectedProfileName),
		),
	)

	if err := form.Run(); err != nil {
		return nil, err
	}

	// Find and return the selected profile
	for _, profile := range profiles {
		if profile.Name == selectedProfileName {
			return &profile, nil
		}
	}

	return nil, fmt.Errorf("selected profile not found")
}

func selectModules(moduleNames []string) ([]string, error) {
	if len(moduleNames) == 0 {
		return nil, fmt.Errorf("no modules available")
	}

	var selectedModules []string
	options := make([]huh.Option[string], len(moduleNames))

	for i, moduleName := range moduleNames {
		options[i] = huh.NewOption(moduleName, moduleName).Selected(true) // All selected by default
	}

	form := huh.NewForm(
		huh.NewGroup(
			huh.NewMultiSelect[string]().
				Title("Select modules to process:").
				Options(options...).
				Value(&selectedModules),
		),
	)

	if err := form.Run(); err != nil {
		return nil, err
	}

	return selectedModules, nil
}

func selectAction() (Action, error) {
	var selectedAction string

	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Select action:").
				Options(
					huh.NewOption(string(ActionCompileAndCopy), string(ActionCompileAndCopy)),
					huh.NewOption(string(ActionCompileOnly), string(ActionCompileOnly)),
					huh.NewOption(string(ActionCopyOnly), string(ActionCopyOnly)),
				).
				Value(&selectedAction),
		),
	)

	if err := form.Run(); err != nil {
		return "", err
	}

	return Action(selectedAction), nil
}

// ============================================================================
// EXECUTION FUNCTIONS
// ============================================================================

func compileModule(moduleName string, module Module, projectPath string) error {
	fullBasePath := filepath.Join(projectPath, module.BasePath)

	fmt.Printf("[COMPILING] %s...\n", moduleName)

	// Check if path exists
	if _, err := os.Stat(fullBasePath); os.IsNotExist(err) {
		return fmt.Errorf("module base path does not exist: %s", fullBasePath)
	}

	// Execute Maven command
	cmd := exec.Command("mvn", "clean", "install", "-DskipTests")
	cmd.Dir = fullBasePath

	// Capture output but don't display it unless there's an error
	output, err := cmd.CombinedOutput()

	if err != nil {
		fmt.Printf("[FAILED] %s compilation failed\n", moduleName)
		fmt.Println("\nMaven output:")
		fmt.Println(string(output))
		return fmt.Errorf("compilation failed for module %s: %w", moduleName, err)
	}

	fmt.Printf("[SUCCESS] %s compiled successfully\n", moduleName)
	return nil
}

func copyArtifact(moduleName string, module Module, projectPath string, deployPath string) error {
	fullArtifactPath := filepath.Join(projectPath, module.ArtifactFile)

	fmt.Printf("[COPYING] %s...\n", moduleName)

	// Check if artifact exists
	if _, err := os.Stat(fullArtifactPath); os.IsNotExist(err) {
		return fmt.Errorf("artifact file does not exist: %s", fullArtifactPath)
	}

	// Get the artifact filename
	artifactName := filepath.Base(fullArtifactPath)
	destPath := filepath.Join(deployPath, artifactName)

	// Create deploy directory if it doesn't exist
	if err := os.MkdirAll(deployPath, 0755); err != nil {
		return fmt.Errorf("failed to create deploy directory: %w", err)
	}

	// Copy the file
	sourceFile, err := os.Open(fullArtifactPath)
	if err != nil {
		return fmt.Errorf("failed to open source file: %w", err)
	}
	defer sourceFile.Close()

	destFile, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("failed to create destination file: %w", err)
	}
	defer destFile.Close()

	if _, err := io.Copy(destFile, sourceFile); err != nil {
		return fmt.Errorf("failed to copy file: %w", err)
	}

	fmt.Printf("[SUCCESS] %s copied to deployments\n", moduleName)
	return nil
}

func executeAction(selectedModules []string, config *Config, action Action, projectPath string, deployPath string) error {
	successCount := 0
	totalCount := len(selectedModules)

	fmt.Println("\n" + strings.Repeat("=", 50))
	fmt.Println("Starting execution...")
	fmt.Println(strings.Repeat("=", 50) + "\n")

	for _, moduleName := range selectedModules {
		module, exists := config.Modules[moduleName]
		if !exists {
			return fmt.Errorf("module not found in config: %s", moduleName)
		}

		// Compile if needed
		if action == ActionCompileOnly || action == ActionCompileAndCopy {
			if err := compileModule(moduleName, module, projectPath); err != nil {
				return err // Stop on first error as per requirements
			}
		}

		// Copy if needed
		if action == ActionCopyOnly || action == ActionCompileAndCopy {
			if err := copyArtifact(moduleName, module, projectPath, deployPath); err != nil {
				return err // Stop on first error as per requirements
			}
		}

		successCount++
		fmt.Println() // Empty line between modules
	}

	// Print summary
	fmt.Println(strings.Repeat("=", 50))
	fmt.Printf("Summary: %d/%d modules processed successfully\n", successCount, totalCount)
	fmt.Println(strings.Repeat("=", 50))

	return nil
}

// ============================================================================
// MAIN FUNCTION
// ============================================================================

func main() {
	configPath := "config.json"

	// Load configuration
	fmt.Println("Loading configuration...")
	config, err := loadConfig(configPath)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		fmt.Printf("Please ensure '%s' exists in the current directory.\n", configPath)
		os.Exit(1)
	}

	// Check if profiles exist
	if len(config.Profiles) == 0 {
		fmt.Println("Error: No profiles configured in config.json")
		os.Exit(1)
	}

	fmt.Println("Configuration loaded successfully\n")

	// Step 1: Select profile
	selectedProfile, err := selectProfile(config.Profiles)
	if err != nil {
		fmt.Printf("Error selecting profile: %v\n", err)
		os.Exit(1)
	}

	// Resolve profile modules (handle inheritance)
	processedProfiles := make(map[string]bool)
	resolvedModules, err := resolveProfileModules(*selectedProfile, config, processedProfiles)
	if err != nil {
		fmt.Printf("Error resolving profile modules: %v\n", err)
		os.Exit(1)
	}

	if len(resolvedModules) == 0 {
		fmt.Println("Error: Selected profile has no modules")
		os.Exit(1)
	}

	// Step 2: Select modules
	selectedModules, err := selectModules(resolvedModules)
	if err != nil {
		fmt.Printf("Error selecting modules: %v\n", err)
		os.Exit(1)
	}

	if len(selectedModules) == 0 {
		fmt.Println("No modules selected. Exiting.")
		os.Exit(0)
	}

	// Step 3: Select action
	selectedAction, err := selectAction()
	if err != nil {
		fmt.Printf("Error selecting action: %v\n", err)
		os.Exit(1)
	}

	// Get effective paths
	projectPath := getEffectiveProjectPath(*selectedProfile, config)
	deployPath := getEffectiveDeployPath(*selectedProfile, config)

	// Execute action
	if err := executeAction(selectedModules, config, selectedAction, projectPath, deployPath); err != nil {
		fmt.Printf("\nError during execution: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("\nExecution completed successfully!")
}
