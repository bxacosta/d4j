package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
)

// ============================================================================
// STRUCTS AND TYPES
// ============================================================================

type Config struct {
	ProjectHome   string              `json:"project_home"`
	JBossHome     string              `json:"jboss_home"`
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
	ModulePath   string `json:"module_path"`
	ArtifactFile string `json:"artifact_file"`
	DeployPath   string `json:"deploy_path,omitempty"`
}

type ProfileModule struct {
	Name       string `json:"name"`
	DeployPath string `json:"deploy_path,omitempty"`
}

type Profile struct {
	Name    string          `json:"name"`
	Modules []ProfileModule `json:"modules"`
}

type Action string

const (
	ActionCopyOnly        Action = "Copy Only"
	ActionCompileOnly     Action = "Compile Only"
	ActionCompileAndCopy  Action = "Compile and Copy"
)

type ModuleStatus int

const (
	StatusInSync     ModuleStatus = iota // Verde - sincronizado
	StatusOutOfSync                      // Amarillo - desincronizado
	StatusMissing                        // Rojo - faltante
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

	// Validate and resolve global deploy_path
	if err := validateVariables(&config); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
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

// resolveVariables resolves all $VAR_NAME$ patterns in a string
// Supports nested variables (e.g., deploy_path = "$jboss_home$/deployments")
// Returns error if circular references detected or variable not found
func resolveVariables(value string, varMap map[string]string) (string, error) {
	const maxIterations = 10 // Prevent infinite loops
	result := value

	for i := 0; i < maxIterations; i++ {
		// Find all $VAR$ patterns
		matches := regexp.MustCompile(`\$([^$]+)\$`).FindAllStringSubmatch(result, -1)
		if len(matches) == 0 {
			return result, nil // No more variables to resolve
		}

		hasReplacement := false
		for _, match := range matches {
			fullMatch := match[0] // e.g., "$project_home$"
			varName := match[1]   // e.g., "project_home"

			replacement, exists := varMap[varName]
			if !exists {
				return "", fmt.Errorf("variable not found: %s", varName)
			}

			result = strings.ReplaceAll(result, fullMatch, replacement)
			hasReplacement = true
		}

		if !hasReplacement {
			break
		}
	}

	// Check if still contains variables (circular reference)
	if strings.Contains(result, "$") {
		return "", fmt.Errorf("circular reference or unresolved variable in: %s", value)
	}

	return result, nil
}

// buildVariableMap creates a map of variable names to values from config
func buildVariableMap(config *Config) map[string]string {
	return map[string]string{
		"project_home": config.ProjectHome,
		"jboss_home":   config.JBossHome,
		"deploy_path":  config.DeployPath,
	}
}

// validateVariables ensures all required global variables are set
func validateVariables(config *Config) error {
	if config.ProjectHome == "" {
		return fmt.Errorf("project_home is required")
	}
	if config.JBossHome == "" {
		return fmt.Errorf("jboss_home is required")
	}
	if config.DeployPath == "" {
		return fmt.Errorf("deploy_path is required")
	}

	// Validate global deploy_path can be resolved
	varMap := buildVariableMap(config)
	resolved, err := resolveVariables(config.DeployPath, varMap)
	if err != nil {
		return fmt.Errorf("failed to resolve global deploy_path: %w", err)
	}

	// Update config with resolved value for efficiency
	config.DeployPath = resolved

	return nil
}

// checkModuleStatus verifies the status of a module by comparing artifact and deployed files
func checkModuleStatus(profileModule ProfileModule, module Module, config *Config) ModuleStatus {
	// Build variable map for path resolution
	varMap := buildVariableMap(config)

	// Resolve artifact file path
	artifactPath, err := resolveVariables(module.ArtifactFile, varMap)
	if err != nil {
		return StatusMissing
	}

	// Resolve deploy path
	deployPath, err := getEffectiveModuleDeployPath(profileModule, module, config)
	if err != nil {
		return StatusMissing
	}

	// Get artifact filename and construct deployed file path
	artifactName := filepath.Base(artifactPath)
	deployedPath := filepath.Join(deployPath, artifactName)

	// Check if artifact file exists
	artifactInfo, errArtifact := os.Stat(artifactPath)
	if errArtifact != nil {
		return StatusMissing // Artifact doesn't exist
	}

	// Check if deployed file exists
	deployedInfo, errDeployed := os.Stat(deployedPath)
	if errDeployed != nil {
		return StatusMissing // Deployed file doesn't exist
	}

	// Compare size
	if artifactInfo.Size() != deployedInfo.Size() {
		return StatusOutOfSync // Different sizes
	}

	// Compare modification time with 2 second tolerance
	timeDiff := artifactInfo.ModTime().Sub(deployedInfo.ModTime())
	if timeDiff < 0 {
		timeDiff = -timeDiff
	}

	if timeDiff > 2*time.Second {
		return StatusOutOfSync // Different times
	}

	return StatusInSync // Files match
}

// getStatusIndicator returns a colored indicator for module status
func getStatusIndicator(status ModuleStatus) string {
	switch status {
	case StatusInSync:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Render("●") // Green
	case StatusOutOfSync:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("226")).Render("●") // Yellow
	case StatusMissing:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Render("●") // Red
	default:
		return "○" // No color
	}
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

// getEffectiveModuleDeployPath determines the effective deploy path using 2-level hierarchy
// Level 1: DeployPath in ProfileModule (most specific)
// Level 2: DeployPath in Module definition
// Level 3: DeployPath global (fallback)
func getEffectiveModuleDeployPath(profileModule ProfileModule, moduleDef Module, config *Config) (string, error) {
	varMap := buildVariableMap(config)

	// Level 1: Module-specific deploy path in ProfileModule
	if profileModule.DeployPath != "" {
		return resolveVariables(profileModule.DeployPath, varMap)
	}

	// Level 2: Deploy path in module definition
	if moduleDef.DeployPath != "" {
		return resolveVariables(moduleDef.DeployPath, varMap)
	}

	// Level 3: Global deploy path (already resolved during loadConfig)
	return config.DeployPath, nil
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

					// Build options with all selected by default and status indicators
					options := make([]huh.Option[string], len(resolvedModules))
					for i, profileModule := range resolvedModules {
						// Get module definition
						module, exists := config.Modules[profileModule.Name]
						if !exists {
							continue
						}

						// Check module status
						status := checkModuleStatus(profileModule, module, config)
						indicator := getStatusIndicator(status)

						// Create label with status indicator
						label := fmt.Sprintf("%s %s", indicator, profileModule.Name)

						options[i] = huh.NewOption(label, profileModule.Name).Selected(true)
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

func compileModule(moduleName string, modulePath string, dryRun bool) error {
	if dryRun {
		fmt.Printf("[DRY-RUN] Would compile %s\n", moduleName)
		fmt.Printf("  Command: mvn clean install -DskipTests\n")
		fmt.Printf("  Working directory: %s\n", modulePath)
		return nil
	}

	fmt.Printf("[COMPILING] %s...\n", moduleName)
	fmt.Println(strings.Repeat("-", 50))

	// Check if path exists
	if _, err := os.Stat(modulePath); os.IsNotExist(err) {
		return fmt.Errorf("module path does not exist: %s", modulePath)
	}

	// Execute Maven command
	cmd := exec.Command("mvn", "clean", "install", "-DskipTests")
	cmd.Dir = modulePath

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

func copyArtifact(moduleName string, artifactFilePath string, deployPath string, dryRun bool) error {
	artifactName := filepath.Base(artifactFilePath)
	destPath := filepath.Join(deployPath, artifactName)

	if dryRun {
		fmt.Printf("[DRY-RUN] Would copy %s\n", moduleName)
		fmt.Printf("  Source: %s\n", artifactFilePath)
		fmt.Printf("  Destination: %s\n", destPath)
		return nil
	}

	fmt.Printf("[COPYING] %s...\n", moduleName)

	// Get source file info to preserve modification time
	sourceInfo, err := os.Stat(artifactFilePath)
	if err != nil {
		return fmt.Errorf("artifact file does not exist: %s", artifactFilePath)
	}

	// Create deploy directory if it doesn't exist
	if err := os.MkdirAll(deployPath, 0755); err != nil {
		return fmt.Errorf("failed to create deploy directory: %w", err)
	}

	// Copy the file
	sourceFile, err := os.Open(artifactFilePath)
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

	// Preserve the modification time from the source file
	if err := os.Chtimes(destPath, sourceInfo.ModTime(), sourceInfo.ModTime()); err != nil {
		return fmt.Errorf("failed to preserve modification time: %w", err)
	}

	fmt.Printf("[SUCCESS] %s copied to deployments\n", moduleName)
	return nil
}

func executeAction(selectedModules []ProfileModule, config *Config, action Action, dryRun bool) error {
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

		// Build variable map
		varMap := buildVariableMap(config)

		// Resolve all paths
		modulePath, err := resolveVariables(module.ModulePath, varMap)
		if err != nil {
			return fmt.Errorf("failed to resolve module_path for %s: %w", profileModule.Name, err)
		}

		artifactPath, err := resolveVariables(module.ArtifactFile, varMap)
		if err != nil {
			return fmt.Errorf("failed to resolve artifact_file for %s: %w", profileModule.Name, err)
		}

		deployPath, err := getEffectiveModuleDeployPath(profileModule, module, config)
		if err != nil {
			return fmt.Errorf("failed to resolve deploy_path for %s: %w", profileModule.Name, err)
		}

		// Compile if needed
		if action == ActionCompileOnly || action == ActionCompileAndCopy {
			if err := compileModule(profileModule.Name, modulePath, dryRun); err != nil {
				return err // Stop on first error as per requirements
			}
		}

		// Copy if needed
		if action == ActionCopyOnly || action == ActionCompileAndCopy {
			if err := copyArtifact(profileModule.Name, artifactPath, deployPath, dryRun); err != nil {
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

	// Execute action
	if err := executeAction(selectedModules, config, Action(selectedAction), *dryRun); err != nil {
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
