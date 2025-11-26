package main

import (
	"encoding/json"
	"flag"
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
	ProjectPath   string              `json:"project_path"`
	DeployPath    string              `json:"deploy_path"`
	Modules       map[string]Module   `json:"modules"`
	Profiles      []Profile           `json:"profiles"`
	LastExecution *LastExecution      `json:"last_execution,omitempty"`
}

type LastExecution struct {
	ProfileName     string          `json:"profile_name"`
	SelectedModules []ProfileModule `json:"selected_modules"`
	Action          string          `json:"action"`
}

type Module struct {
	BasePath     string `json:"base_path"`
	ArtifactFile string `json:"artifact_file"`
}

type ProfileModule struct {
	Name       string `json:"name"`
	DeployPath string `json:"deploy_path,omitempty"`
}

type Profile struct {
	Name        string          `json:"name"`
	ProjectPath string          `json:"project_path"`
	DeployPath  string          `json:"deploy_path"`
	Modules     []ProfileModule `json:"modules"`
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

func saveConfig(configPath string, config *Config) error {
	bytes, err := json.MarshalIndent(config, "", "    ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	if err := os.WriteFile(configPath, bytes, 0644); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	return nil
}

func saveLastExecution(configPath string, config *Config, profileName string, selectedModules []ProfileModule, action string) error {
	config.LastExecution = &LastExecution{
		ProfileName:     profileName,
		SelectedModules: selectedModules,
		Action:          action,
	}

	return saveConfig(configPath, config)
}

func resolveProfileModules(profile Profile, config *Config, processedProfiles map[string]bool) ([]ProfileModule, error) {
	if processedProfiles[profile.Name] {
		return nil, fmt.Errorf("circular profile reference detected: %s", profile.Name)
	}
	processedProfiles[profile.Name] = true

	var resolvedModules []ProfileModule

	for _, profileModule := range profile.Modules {
		// Check if it's a profile reference ($PROFILE_NAME$)
		if strings.HasPrefix(profileModule.Name, "$") && strings.HasSuffix(profileModule.Name, "$") {
			referencedProfileName := strings.Trim(profileModule.Name, "$")

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

			// If the current profileModule has a deploy_path, override it for all inherited modules
			if profileModule.DeployPath != "" {
				for i := range referencedModules {
					referencedModules[i].DeployPath = profileModule.DeployPath
				}
			}

			resolvedModules = append(resolvedModules, referencedModules...)
		} else {
			// It's a regular module
			if _, exists := config.Modules[profileModule.Name]; !exists {
				return nil, fmt.Errorf("module not found in config: %s", profileModule.Name)
			}
			resolvedModules = append(resolvedModules, profileModule)
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

func getEffectiveModuleDeployPath(profileModule ProfileModule, profile Profile, config *Config) string {
	// Level 1: Module-specific deploy path (most specific)
	if profileModule.DeployPath != "" {
		return profileModule.DeployPath
	}

	// Level 2: Profile deploy path
	if profile.DeployPath != "" {
		return profile.DeployPath
	}

	// Level 3: Global deploy path (least specific)
	return config.DeployPath
}

// ============================================================================
// UX FUNCTIONS (HUH INTERACTIVE FORMS)
// ============================================================================

type FormData struct {
	SelectedProfileName string
	SelectedModuleNames []string
	SelectedModules     []ProfileModule
	SelectedAction      string
}

func runInteractiveForm(config *Config) (*FormData, error) {
	if len(config.Profiles) == 0 {
		return nil, fmt.Errorf("no profiles configured")
	}

	var formData FormData

	// Build profile options
	profileOptions := make([]huh.Option[string], len(config.Profiles))
	for i, profile := range config.Profiles {
		profileOptions[i] = huh.NewOption(profile.Name, profile.Name)
	}

	// Create multi-step form with backward navigation
	form := huh.NewForm(
		// Step 1: Select profile
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Select a profile:").
				Options(profileOptions...).
				Value(&formData.SelectedProfileName),
		),

		// Step 2: Select modules (dynamic based on selected profile)
		huh.NewGroup(
			huh.NewMultiSelect[string]().
				Title("Select modules to process:").
				OptionsFunc(func() []huh.Option[string] {
					// Find the selected profile
					var selectedProfile *Profile
					for _, p := range config.Profiles {
						if p.Name == formData.SelectedProfileName {
							selectedProfile = &p
							break
						}
					}

					if selectedProfile == nil {
						return []huh.Option[string]{}
					}

					// Resolve modules for the selected profile
					processedProfiles := make(map[string]bool)
					resolvedModules, err := resolveProfileModules(*selectedProfile, config, processedProfiles)
					if err != nil {
						return []huh.Option[string]{}
					}

					// Build options with all selected by default
					options := make([]huh.Option[string], len(resolvedModules))
					for i, profileModule := range resolvedModules {
						options[i] = huh.NewOption(profileModule.Name, profileModule.Name).Selected(true)
					}

					return options
				}, &formData.SelectedProfileName). // Re-evaluate when profile changes
				Value(&formData.SelectedModuleNames),
		),

		// Step 3: Select action
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Select action:").
				Options(
					huh.NewOption(string(ActionCompileAndCopy), string(ActionCompileAndCopy)),
					huh.NewOption(string(ActionCompileOnly), string(ActionCompileOnly)),
					huh.NewOption(string(ActionCopyOnly), string(ActionCopyOnly)),
				).
				Value(&formData.SelectedAction),
		),
	).WithTheme(huh.ThemeBase())

	if err := form.Run(); err != nil {
		return nil, err
	}

	// Convert selected module names to ProfileModule objects
	var selectedProfile *Profile
	for _, p := range config.Profiles {
		if p.Name == formData.SelectedProfileName {
			selectedProfile = &p
			break
		}
	}

	if selectedProfile == nil {
		return nil, fmt.Errorf("selected profile not found")
	}

	// Resolve modules for the selected profile
	processedProfiles := make(map[string]bool)
	resolvedModules, err := resolveProfileModules(*selectedProfile, config, processedProfiles)
	if err != nil {
		return nil, err
	}

	// Filter to only include selected modules
	for _, selectedName := range formData.SelectedModuleNames {
		for _, profileModule := range resolvedModules {
			if profileModule.Name == selectedName {
				formData.SelectedModules = append(formData.SelectedModules, profileModule)
				break
			}
		}
	}

	return &formData, nil
}

// ============================================================================
// EXECUTION FUNCTIONS
// ============================================================================

func compileModule(moduleName string, module Module, projectPath string, dryRun bool) error {
	fullBasePath := filepath.Join(projectPath, module.BasePath)

	if dryRun {
		fmt.Printf("[DRY-RUN] Would compile %s\n", moduleName)
		fmt.Printf("  Command: mvn clean install -DskipTests\n")
		fmt.Printf("  Working directory: %s\n", fullBasePath)
		return nil
	}

	fmt.Printf("[COMPILING] %s...\n", moduleName)
	fmt.Println(strings.Repeat("-", 50))

	// Check if path exists
	if _, err := os.Stat(fullBasePath); os.IsNotExist(err) {
		return fmt.Errorf("module base path does not exist: %s", fullBasePath)
	}

	// Execute Maven command
	cmd := exec.Command("mvn", "clean", "install", "-DskipTests")
	cmd.Dir = fullBasePath

	// Show Maven output in real-time
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()

	if err != nil {
		fmt.Println(strings.Repeat("-", 50))
		fmt.Printf("[FAILED] %s compilation failed\n", moduleName)
		return fmt.Errorf("compilation failed for module %s: %w", moduleName, err)
	}

	fmt.Println(strings.Repeat("-", 50))
	fmt.Printf("[SUCCESS] %s compiled successfully\n", moduleName)
	return nil
}

func copyArtifact(moduleName string, module Module, projectPath string, deployPath string, dryRun bool) error {
	fullArtifactPath := filepath.Join(projectPath, module.ArtifactFile)
	artifactName := filepath.Base(fullArtifactPath)
	destPath := filepath.Join(deployPath, artifactName)

	if dryRun {
		fmt.Printf("[DRY-RUN] Would copy %s\n", moduleName)
		fmt.Printf("  Source: %s\n", fullArtifactPath)
		fmt.Printf("  Destination: %s\n", destPath)
		return nil
	}

	fmt.Printf("[COPYING] %s...\n", moduleName)

	// Check if artifact exists
	if _, err := os.Stat(fullArtifactPath); os.IsNotExist(err) {
		return fmt.Errorf("artifact file does not exist: %s", fullArtifactPath)
	}

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

func executeAction(selectedModules []ProfileModule, config *Config, action Action, profile Profile, projectPath string, dryRun bool) error {
	successCount := 0
	totalCount := len(selectedModules)

	fmt.Println("\n" + strings.Repeat("=", 50))
	if dryRun {
		fmt.Println("DRY-RUN MODE - No actual changes will be made")
	} else {
		fmt.Println("Starting execution...")
	}
	fmt.Println(strings.Repeat("=", 50) + "\n")

	for _, profileModule := range selectedModules {
		module, exists := config.Modules[profileModule.Name]
		if !exists {
			return fmt.Errorf("module not found in config: %s", profileModule.Name)
		}

		// Get the effective deploy path for this module
		moduleDeployPath := getEffectiveModuleDeployPath(profileModule, profile, config)

		// Compile if needed
		if action == ActionCompileOnly || action == ActionCompileAndCopy {
			if err := compileModule(profileModule.Name, module, projectPath, dryRun); err != nil {
				return err // Stop on first error as per requirements
			}
		}

		// Copy if needed
		if action == ActionCopyOnly || action == ActionCompileAndCopy {
			if err := copyArtifact(profileModule.Name, module, projectPath, moduleDeployPath, dryRun); err != nil {
				return err // Stop on first error as per requirements
			}
		}

		successCount++
		fmt.Println() // Empty line between modules
	}

	// Print summary
	fmt.Println(strings.Repeat("=", 50))
	if dryRun {
		fmt.Printf("DRY-RUN Summary: %d/%d modules would be processed\n", successCount, totalCount)
	} else {
		fmt.Printf("Summary: %d/%d modules processed successfully\n", successCount, totalCount)
	}
	fmt.Println(strings.Repeat("=", 50))

	return nil
}

// ============================================================================
// MAIN FUNCTION
// ============================================================================

func main() {
	// Parse command line flags
	dryRun := flag.Bool("dry-run", false, "Run in dry-run mode (show what would be done without executing)")
	flag.BoolVar(dryRun, "d", false, "Run in dry-run mode (shorthand)")
	last := flag.Bool("last", false, "Re-run the last execution")
	flag.BoolVar(last, "l", false, "Re-run the last execution (shorthand)")
	flag.Parse()

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

	fmt.Println("Configuration loaded successfully")
	if *dryRun {
		fmt.Println("Running in DRY-RUN mode - no actual changes will be made")
	}
	fmt.Println()

	var selectedProfileName string
	var selectedModules []ProfileModule
	var selectedAction string

	// Check if re-running last execution
	if *last {
		if config.LastExecution == nil || len(config.LastExecution.SelectedModules) == 0 {
			fmt.Println("Error: No previous execution found")
			fmt.Println("Run the tool normally first to save an execution")
			os.Exit(1)
		}

		// Build module names list for display
		moduleNames := make([]string, len(config.LastExecution.SelectedModules))
		for i, pm := range config.LastExecution.SelectedModules {
			moduleNames[i] = pm.Name
		}

		fmt.Println("Re-running last execution:")
		fmt.Printf("  Profile: %s\n", config.LastExecution.ProfileName)
		fmt.Printf("  Modules: %s\n", strings.Join(moduleNames, ", "))
		fmt.Printf("  Action: %s\n", config.LastExecution.Action)
		fmt.Println()

		selectedProfileName = config.LastExecution.ProfileName
		selectedModules = config.LastExecution.SelectedModules
		selectedAction = config.LastExecution.Action
	} else {
		// Run interactive form with backward navigation
		formData, err := runInteractiveForm(config)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}

		// Validate selections
		if len(formData.SelectedModules) == 0 {
			fmt.Println("No modules selected. Exiting.")
			os.Exit(0)
		}

		selectedProfileName = formData.SelectedProfileName
		selectedModules = formData.SelectedModules
		selectedAction = formData.SelectedAction
	}

	// Find the selected profile
	var selectedProfile *Profile
	for _, p := range config.Profiles {
		if p.Name == selectedProfileName {
			selectedProfile = &p
			break
		}
	}

	if selectedProfile == nil {
		fmt.Println("Error: Selected profile not found")
		os.Exit(1)
	}

	// Get effective project path
	projectPath := getEffectiveProjectPath(*selectedProfile, config)

	// Execute action
	if err := executeAction(selectedModules, config, Action(selectedAction), *selectedProfile, projectPath, *dryRun); err != nil {
		fmt.Printf("\nError during execution: %v\n", err)
		os.Exit(1)
	}

	// Save last execution (only if not in dry-run mode and not already running last)
	if !*dryRun && !*last {
		if err := saveLastExecution(configPath, config, selectedProfileName, selectedModules, selectedAction); err != nil {
			fmt.Printf("Warning: Failed to save last execution: %v\n", err)
		}
	}

	if *dryRun {
		fmt.Println("\nDry-run completed successfully!")
	} else {
		fmt.Println("\nExecution completed successfully!")
	}
}
