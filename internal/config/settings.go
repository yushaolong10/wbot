package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

type Profile struct {
	Version            int                                   `yaml:"version"`
	Identity           struct{ Name, Role, Language string } `yaml:"identity"`
	Personality        struct{ Tone, Initiative string }     `yaml:"personality"`
	WorkingStyle       map[string]any                        `yaml:"working_style"`
	Memory             map[string]any                        `yaml:"memory"`
	Constraints        map[string]any                        `yaml:"constraints"`
	CustomInstructions string                                `yaml:"custom_instructions"`
}

type Model struct {
	BaseURL, Name, APIKey     string
	Thinking, ReasoningEffort string
	MaxOutputTokens           int
	Timeout                   time.Duration
}

type Settings struct {
	Addr, DataRoot, DatabasePath, WorkspaceRoot, PermissionMode, AuthToken, ProfilePath string
	AllowShell, AllowNetwork, AllowExternalWrite                                        bool
	MaxParallelism                                                                      int
	MaxContextTokens                                                                    int
	AdvisorMaxCalls                                                                     int
	DefaultModel, AdvisorModel                                                          Model
}

func Load() (Settings, error) {
	cwd, _ := os.Getwd()
	data := env("WBOT_DATA_ROOT", filepath.Join(cwd, ".wbot-data"))
	modelAPIKey := os.Getenv("WBOT_MODEL_API_KEY")
	s := Settings{
		Addr: env("WBOT_ADDR", "127.0.0.1:8080"), DataRoot: data,
		DatabasePath:  env("WBOT_DATABASE_PATH", filepath.Join(data, "wbot.db")),
		WorkspaceRoot: env("WBOT_WORKSPACE_ROOT", cwd), PermissionMode: env("WBOT_PERMISSION_MODE", "approval"),
		AuthToken: os.Getenv("WBOT_AUTH_TOKEN"), ProfilePath: env("WBOT_PROFILE", filepath.Join(cwd, "profiles", "default.yaml")),
		AllowShell: envBool("WBOT_ALLOW_SHELL", true), AllowNetwork: envBool("WBOT_ALLOW_NETWORK", true),
		AllowExternalWrite: envBool("WBOT_ALLOW_EXTERNAL_WRITE", false), MaxParallelism: envInt("WBOT_TASK_MAX_PARALLELISM", 4),
		MaxContextTokens: envInt("WBOT_MAX_CONTEXT_TOKENS", 60000),
		AdvisorMaxCalls:  envInt("WBOT_ADVISOR_MAX_CALLS_PER_TASK", 3),
		DefaultModel:     Model{BaseURL: env("WBOT_MODEL_BASE_URL", "https://api.deepseek.com"), Name: env("WBOT_DEFAULT_MODEL", "deepseek-v4-flash"), APIKey: modelAPIKey, MaxOutputTokens: envInt("WBOT_DEFAULT_MAX_OUTPUT_TOKENS", 16000), Timeout: time.Duration(envInt("WBOT_MODEL_TIMEOUT_SECONDS", 120)) * time.Second},
		AdvisorModel:     Model{BaseURL: env("WBOT_ADVISOR_BASE_URL", env("WBOT_MODEL_BASE_URL", "https://api.deepseek.com")), Name: env("WBOT_ADVISOR_MODEL", "deepseek-v4-pro"), APIKey: modelAPIKey, Thinking: "enabled", ReasoningEffort: "max", MaxOutputTokens: envInt("WBOT_ADVISOR_MAX_OUTPUT_TOKENS", 32000), Timeout: time.Duration(envInt("WBOT_ADVISOR_TIMEOUT_SECONDS", 180)) * time.Second},
	}
	if s.PermissionMode != "approval" && s.PermissionMode != "full_access" {
		return s, fmt.Errorf("invalid WBOT_PERMISSION_MODE %q", s.PermissionMode)
	}
	if s.MaxParallelism < 1 {
		return s, errors.New("WBOT_TASK_MAX_PARALLELISM must be positive")
	}
	for _, k := range []string{"WBOT_ALLOW_SHELL", "WBOT_ALLOW_NETWORK", "WBOT_ALLOW_EXTERNAL_WRITE"} {
		if v := os.Getenv(k); v != "" {
			if _, e := strconv.ParseBool(v); e != nil {
				return s, fmt.Errorf("%s must be a boolean: %w", k, e)
			}
		}
	}
	for _, k := range []string{"WBOT_TASK_MAX_PARALLELISM", "WBOT_MAX_CONTEXT_TOKENS", "WBOT_ADVISOR_MAX_CALLS_PER_TASK"} {
		if v := os.Getenv(k); v != "" {
			if _, e := strconv.Atoi(v); e != nil {
				return s, fmt.Errorf("%s must be an integer: %w", k, e)
			}
		}
	}
	for _, p := range []string{s.DataRoot, filepath.Dir(s.DatabasePath), filepath.Join(s.DataRoot, "artifacts"), filepath.Join(s.DataRoot, "memory")} {
		if err := os.MkdirAll(p, 0700); err != nil {
			return s, err
		}
	}
	abs, err := filepath.Abs(s.WorkspaceRoot)
	if err != nil {
		return s, err
	}
	s.WorkspaceRoot = abs
	return s, nil
}

func LoadProfile(path string) (Profile, []byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Profile{}, nil, err
	}
	var p Profile
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		return p, b, err
	}
	if p.Version != 1 || p.Identity.Name == "" {
		return p, b, errors.New("profile version must be 1 and identity.name is required")
	}
	return p, b, nil
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func envBool(k string, d bool) bool {
	v := os.Getenv(k)
	if v == "" {
		return d
	}
	b, e := strconv.ParseBool(v)
	if e != nil {
		return d
	}
	return b
}
func envInt(k string, d int) int {
	v := os.Getenv(k)
	if v == "" {
		return d
	}
	n, e := strconv.Atoi(v)
	if e != nil {
		return d
	}
	return n
}
