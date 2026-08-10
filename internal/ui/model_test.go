package ui

import (
	"context"
	"testing"

	"github.com/daintreehq/assistant/internal/app"
	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/domain"
)

func TestNewModelUsesStableProjectPlaceholder(t *testing.T) {
	a := &app.App{Config: config.AppConfig{
		ProjectPath: "/tmp/6f2d9a8c",
		Tier:        domain.TierSystem,
	}}
	m := newModel(context.Background(), a, newEventPump())
	if m.masthead.ProjectName != projectNamePlaceholder {
		t.Fatalf("project placeholder = %q, want %q", m.masthead.ProjectName, projectNamePlaceholder)
	}
	if m.masthead.ProjectName == "6f2d9a8c" {
		t.Fatal("masthead must not expose the hashed project-path basename as a project name")
	}
}
