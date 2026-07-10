package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/anomalysh/rift/projects/server/internal/config"
)

// runConfig implements the `riftd config <subcommand>` tooling surface. It is
// dispatched from main() before any server startup, so `riftd` with no
// subcommand stays byte-for-byte the server it always was. The point is a single
// source of truth: operator scripts (setup.sh, harden.sh, the e2e wizard test)
// validate and read defaults through the real config package instead of
// re-encoding its rules in shell.
func runConfig(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: riftd config <validate|defaults> [--env-file FILE]")
	}
	switch args[0] {
	case "validate":
		return configValidate(args[1:])
	case "defaults":
		return configDefaults(args[1:])
	default:
		return fmt.Errorf("unknown subcommand %q (want: validate, defaults)", args[0])
	}
}

// configValidate loads the configuration (optionally after applying an env file)
// and reports whether it is valid, WITHOUT connecting to Postgres or starting a
// listener. Exit 0 means "this .env would boot"; a non-zero exit carries the
// same error the server would print.
func configValidate(args []string) error {
	var envFile string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--env-file":
			if i+1 >= len(args) {
				return errors.New("--env-file needs a path")
			}
			envFile = args[i+1]
			i++
		default:
			return fmt.Errorf("unexpected argument %q", args[i])
		}
	}

	if envFile != "" {
		if err := applyEnvFile(envFile); err != nil {
			return err
		}
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	for _, w := range cfg.Warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}
	fmt.Printf("config valid (env=%s)\n", cfg.Env)
	return nil
}

// configDefaults prints the operator-relevant default environment variables, one
// KEY=value per line, so a shell tool can source them.
func configDefaults(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("defaults takes no arguments (got %q)", args[0])
	}
	for _, d := range config.EnvDefaults() {
		fmt.Printf("%s=%s\n", d.Key, d.Value)
	}
	return nil
}

// applyEnvFile reads a dotenv-style file (KEY=VALUE lines, # comments, optional
// surrounding quotes) and sets each entry in the process environment so a
// following config.Load sees it. Matches how docker compose reads an env file;
// it is not a shell, so it does no interpolation.
func applyEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open env file: %w", err)
	}
	defer func() { _ = f.Close() }()

	scan := bufio.NewScanner(f)
	line := 0
	for scan.Scan() {
		line++
		raw := strings.TrimSpace(scan.Text())
		if raw == "" || strings.HasPrefix(raw, "#") {
			continue
		}
		key, val, ok := strings.Cut(raw, "=")
		if !ok {
			return fmt.Errorf("%s:%d: not KEY=VALUE: %q", path, line, raw)
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		// Strip one layer of matching quotes, as compose's env-file reader does.
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		if err := os.Setenv(key, val); err != nil {
			return fmt.Errorf("set %s: %w", key, err)
		}
	}
	if err := scan.Err(); err != nil {
		return fmt.Errorf("read env file: %w", err)
	}
	return nil
}
