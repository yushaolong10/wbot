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

type MemoryRetrieval struct {
	MaxEntries, MaxTokens, MaxEntryTokens int
	MinScore                              float64
	UseFTS, UseEmbeddings, UseLLMRerank   bool
}
type MemoryWrite struct {
	MinConfidence                    float64
	RequireEvidence, AutoConsolidate bool
}
type MemoryRetention struct {
	EpisodicTTLDays, DeletedRetentionDays, VersionRetentionCount, StaleAfterDays int
	EnablePhysicalGC                                                             bool
}
type MemorySettings struct {
	Enabled, AutoExtract bool
	Retrieval            MemoryRetrieval
	Write                MemoryWrite
	Retention            MemoryRetention
}
type HistorySettings struct {
	MaxLoadedMessages, RecentMessages, RecentMinMessages               int
	SegmentMessages, SegmentMaxSourceTokens, SegmentMergeFactor        int
	SummaryTargetTokens, ToolSnapshotMaxTokens, ReactiveRecentMessages int
	ReactiveRetryCount                                                 int
}
type ContextSettings struct {
	SafetyMarginTokens, OutputReserveTokens int
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
	Memory                                                                              MemorySettings
	History                                                                             HistorySettings
	Context                                                                             ContextSettings
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
		Memory: MemorySettings{Enabled: envBool("WBOT_MEMORY_ENABLED", true), AutoExtract: envBool("WBOT_MEMORY_AUTO_EXTRACT", true),
			Retrieval: MemoryRetrieval{MaxEntries: envInt("WBOT_MEMORY_MAX_ENTRIES", 8), MaxTokens: envInt("WBOT_MEMORY_MAX_TOKENS", 6000), MaxEntryTokens: envInt("WBOT_MEMORY_MAX_ENTRY_TOKENS", 800), MinScore: .35, UseFTS: true, UseLLMRerank: envBool("WBOT_MEMORY_LLM_RERANK", true)},
			Write:     MemoryWrite{MinConfidence: .8, RequireEvidence: true, AutoConsolidate: true},
			Retention: MemoryRetention{EpisodicTTLDays: 90, DeletedRetentionDays: 30, VersionRetentionCount: 10, StaleAfterDays: 180, EnablePhysicalGC: true}},
		History: HistorySettings{MaxLoadedMessages: envInt("WBOT_HISTORY_MAX_LOADED", 500), RecentMessages: envInt("WBOT_HISTORY_RECENT_MESSAGES", 20), RecentMinMessages: 4, SegmentMessages: envInt("WBOT_HISTORY_SEGMENT_MESSAGES", 30), SegmentMaxSourceTokens: envInt("WBOT_HISTORY_SEGMENT_MAX_TOKENS", 12000), SegmentMergeFactor: 4, SummaryTargetTokens: envInt("WBOT_HISTORY_SUMMARY_TOKENS", 800), ToolSnapshotMaxTokens: envInt("WBOT_TOOL_SNAPSHOT_MAX_TOKENS", 1200), ReactiveRecentMessages: 6, ReactiveRetryCount: envInt("WBOT_HISTORY_REACTIVE_RETRY_COUNT", 1)},
		Context: ContextSettings{SafetyMarginTokens: envInt("WBOT_CONTEXT_SAFETY_MARGIN", 2000), OutputReserveTokens: envInt("WBOT_DEFAULT_MAX_OUTPUT_TOKENS", 16000)},
	}
	if s.PermissionMode != "approval" && s.PermissionMode != "full_access" {
		return s, fmt.Errorf("invalid WBOT_PERMISSION_MODE %q", s.PermissionMode)
	}
	if s.MaxParallelism < 1 {
		return s, errors.New("WBOT_TASK_MAX_PARALLELISM must be positive")
	}
	positive := map[string]int{
		"WBOT_MAX_CONTEXT_TOKENS":           s.MaxContextTokens,
		"WBOT_DEFAULT_MAX_OUTPUT_TOKENS":    s.DefaultModel.MaxOutputTokens,
		"WBOT_CONTEXT_SAFETY_MARGIN":        s.Context.SafetyMarginTokens,
		"WBOT_MEMORY_MAX_ENTRIES":           s.Memory.Retrieval.MaxEntries,
		"WBOT_MEMORY_MAX_TOKENS":            s.Memory.Retrieval.MaxTokens,
		"WBOT_MEMORY_MAX_ENTRY_TOKENS":      s.Memory.Retrieval.MaxEntryTokens,
		"WBOT_HISTORY_MAX_LOADED":           s.History.MaxLoadedMessages,
		"WBOT_HISTORY_RECENT_MESSAGES":      s.History.RecentMessages,
		"WBOT_HISTORY_SEGMENT_MESSAGES":     s.History.SegmentMessages,
		"WBOT_HISTORY_SEGMENT_MAX_TOKENS":   s.History.SegmentMaxSourceTokens,
		"WBOT_HISTORY_SUMMARY_TOKENS":       s.History.SummaryTargetTokens,
		"WBOT_TOOL_SNAPSHOT_MAX_TOKENS":     s.History.ToolSnapshotMaxTokens,
		"WBOT_HISTORY_REACTIVE_RETRY_COUNT": s.History.ReactiveRetryCount,
	}
	for name, value := range positive {
		if value <= 0 {
			return s, fmt.Errorf("%s must be positive", name)
		}
	}
	if s.Context.OutputReserveTokens+s.Context.SafetyMarginTokens >= s.MaxContextTokens {
		return s, errors.New("output reserve plus safety margin must be smaller than model context window")
	}
	if s.History.ReactiveRetryCount != 1 {
		return s, errors.New("WBOT_HISTORY_REACTIVE_RETRY_COUNT must be 1")
	}
	if !s.Memory.Enabled {
		s.Memory.AutoExtract = false
	}
	for _, k := range []string{"WBOT_ALLOW_SHELL", "WBOT_ALLOW_NETWORK", "WBOT_ALLOW_EXTERNAL_WRITE", "WBOT_MEMORY_ENABLED", "WBOT_MEMORY_AUTO_EXTRACT", "WBOT_MEMORY_LLM_RERANK"} {
		if v := os.Getenv(k); v != "" {
			if _, e := strconv.ParseBool(v); e != nil {
				return s, fmt.Errorf("%s must be a boolean: %w", k, e)
			}
		}
	}
	for _, k := range []string{"WBOT_TASK_MAX_PARALLELISM", "WBOT_MAX_CONTEXT_TOKENS", "WBOT_DEFAULT_MAX_OUTPUT_TOKENS", "WBOT_ADVISOR_MAX_CALLS_PER_TASK", "WBOT_MEMORY_MAX_ENTRIES", "WBOT_MEMORY_MAX_TOKENS", "WBOT_MEMORY_MAX_ENTRY_TOKENS", "WBOT_HISTORY_MAX_LOADED", "WBOT_HISTORY_RECENT_MESSAGES", "WBOT_HISTORY_SEGMENT_MESSAGES", "WBOT_HISTORY_SEGMENT_MAX_TOKENS", "WBOT_HISTORY_SUMMARY_TOKENS", "WBOT_TOOL_SNAPSHOT_MAX_TOKENS", "WBOT_HISTORY_REACTIVE_RETRY_COUNT", "WBOT_CONTEXT_SAFETY_MARGIN"} {
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
