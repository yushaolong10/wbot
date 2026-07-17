package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProfileRejectsUnknownField(t *testing.T) {
	p := filepath.Join(t.TempDir(), "p.yaml")
	os.WriteFile(p, []byte("version: 1\nidentity:\n  name: x\nunknown: true\n"), 0600)
	if _, _, e := LoadProfile(p); e == nil {
		t.Fatal("unknown profile field accepted")
	}
}
func TestInvalidEnvironmentRejected(t *testing.T) {
	t.Setenv("WBOT_ALLOW_SHELL", "maybe")
	if _, e := Load(); e == nil {
		t.Fatal("invalid boolean accepted")
	}
}

func TestGenericModelAPIKeyIsApplied(t *testing.T) {
	root := t.TempDir()
	t.Setenv("WBOT_DATA_ROOT", filepath.Join(root, "data"))
	t.Setenv("WBOT_WORKSPACE_ROOT", root)
	t.Setenv("WBOT_MODEL_API_KEY", "generic")
	s, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if s.DefaultModel.APIKey != "generic" || s.AdvisorModel.APIKey != "generic" {
		t.Fatalf("generic model key was not applied: default=%q advisor=%q", s.DefaultModel.APIKey, s.AdvisorModel.APIKey)
	}
}

func TestAdvisorModelEnvironment(t *testing.T) {
	root := t.TempDir()
	t.Setenv("WBOT_DATA_ROOT", filepath.Join(root, "data"))
	t.Setenv("WBOT_WORKSPACE_ROOT", root)
	t.Setenv("WBOT_ADVISOR_MODEL", "custom-advisor-model")
	s, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if s.AdvisorModel.Name != "custom-advisor-model" {
		t.Fatalf("advisor model=%q", s.AdvisorModel.Name)
	}
}

func TestAdvisorBaseURLDefaultsToModelBaseURL(t *testing.T) {
	root := t.TempDir()
	t.Setenv("WBOT_DATA_ROOT", filepath.Join(root, "data"))
	t.Setenv("WBOT_WORKSPACE_ROOT", root)
	t.Setenv("WBOT_MODEL_BASE_URL", "https://model.example.com")
	t.Setenv("WBOT_ADVISOR_BASE_URL", "")
	s, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if s.AdvisorModel.BaseURL != "https://model.example.com" {
		t.Fatalf("advisor base URL=%q", s.AdvisorModel.BaseURL)
	}
}

func TestAdvisorBaseURLCanBeOverridden(t *testing.T) {
	root := t.TempDir()
	t.Setenv("WBOT_DATA_ROOT", filepath.Join(root, "data"))
	t.Setenv("WBOT_WORKSPACE_ROOT", root)
	t.Setenv("WBOT_MODEL_BASE_URL", "https://model.example.com")
	t.Setenv("WBOT_ADVISOR_BASE_URL", "https://advisor.example.com")
	s, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if s.AdvisorModel.BaseURL != "https://advisor.example.com" {
		t.Fatalf("advisor base URL=%q", s.AdvisorModel.BaseURL)
	}
}
