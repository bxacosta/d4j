# D4J - Deployment Tool for Java/JBoss

CLI tool for compiling and deploying Java EE modules to JBoss.

## Requirements

- Go 1.23+
- Maven
- Java 8+

## Setup

1. Copy `config.example.json` to `config.json`
2. Update paths and modules in `config.json`
3. Build: `go build -o d4j main.go`

## Usage

### Normal Mode

Run `./d4j` and follow the interactive prompts:

1. Select a profile
2. Select modules to process
3. Choose action (Copy Only / Compile Only / Compile and Copy)

### Dry-Run Mode

Test the tool without making actual changes:

```bash
./d4j --dry-run
# or
./d4j -d
```

In dry-run mode, the tool will show exactly what commands would be executed and which files would be copied, without actually running Maven or copying any files.

## Configuration

See `config.example.json` for structure.

### Profile Inheritance

Use `$PROFILE_NAME$` to include modules from another profile:

```json
{
    "name": "SERVICES",
    "modules": ["$BASE$", "services-ear"]
}
```

## License

This project is licensed under the [MIT License](LICENSE).