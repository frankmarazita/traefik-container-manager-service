package main

import (
	"testing"

	"github.com/docker/docker/api/types"
)

func labelled(labels map[string]string) types.Container {
	return types.Container{Labels: labels}
}

func TestPrefixMatch(t *testing.T) {
	cases := []struct {
		name      string
		requested string
		label     string
		want      bool
	}{
		{"empty requested never matches", "", "app.example.com", false},
		{"empty label never matches", "app.example.com", "", false},
		{"both empty never matches", "", "", false},
		{"exact match", "app.example.com", "app.example.com", true},
		{"requested extends label", "app.example.com/health", "app.example.com", true},
		{"label extends requested", "app.example.com", "app.example.com/health", true},
		{"unrelated values", "other.example.com", "app.example.com", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := prefixMatch(tc.requested, tc.label); got != tc.want {
				t.Errorf("prefixMatch(%q, %q) = %v, want %v", tc.requested, tc.label, got, tc.want)
			}
		})
	}
}

func TestServiceMatchesIgnoresForeignContainers(t *testing.T) {
	service := &Service{name: "myapp"}
	foreign := labelled(map[string]string{
		"traefik-container-manager.name": "other",
		"traefik-container-manager.host": "other.example.com",
		"traefik-container-manager.path": "/other",
	})

	if service.matches(foreign) {
		t.Error("service with no host or path must not match a foreign container's host/path labels")
	}
}

func TestServiceMatchesByName(t *testing.T) {
	service := &Service{name: "myapp"}
	own := labelled(map[string]string{"traefik-container-manager.name": "MyApp"})

	if !service.matches(own) {
		t.Error("expected case-insensitive match on the name label")
	}
}

func TestServiceMatchesFallsThroughToPath(t *testing.T) {
	service := &Service{name: "generic", host: "wrong.example.com", path: "/app"}
	container := labelled(map[string]string{
		"traefik-container-manager.name": "other",
		"traefik-container-manager.host": "app.example.com",
		"traefik-container-manager.path": "/app",
	})

	if !service.matches(container) {
		t.Error("a non-matching host label must not stop the path label from being checked")
	}
}

func TestServiceMatchesEmptyNameLabel(t *testing.T) {
	service := &Service{}
	container := labelled(map[string]string{"traefik-container-manager.name": ""})

	if service.matches(container) {
		t.Error("an empty service name must not match an empty name label")
	}
}
