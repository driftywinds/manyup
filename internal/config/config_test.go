package config

import "testing"

func TestServiceOptionHelpers(t *testing.T) {
	cfg := DefaultConfig()

	if _, ok := cfg.Service("interim@friends"); ok {
		t.Fatal("a service should not exist before it is configured")
	}

	cfg.SetOption("interim@friends", "BASE_URL", "https://friends.example.com")
	sc, ok := cfg.Service("interim@friends")
	if !ok {
		t.Fatal("SetOption did not create the service entry")
	}
	if got, want := sc.Options["BASE_URL"], "https://friends.example.com"; got != want {
		t.Errorf("BASE_URL = %q, want %q", got, want)
	}
	if !sc.Enabled {
		t.Error("a new service entry should be enabled")
	}

	cfg.SetOption("interim@friends", "BASE_URL", "")
	if got := sc.Options["BASE_URL"]; got != "" {
		t.Errorf("an empty option should be removed, got %q", got)
	}

	cfg.SetCredential("interim@friends", "API_TOKEN", "interim_test")
	if got, want := sc.Credentials["API_TOKEN"], "interim_test"; got != want {
		t.Errorf("API_TOKEN = %q, want %q", got, want)
	}
}

func TestServiceSelectionHelpers(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.IsSelected("interim@friends") {
		t.Fatal("a new config should have no selected services")
	}
	if !cfg.SelectService("interim@friends") {
		t.Error("SelectService should report a change")
	}
	if cfg.SelectService("interim@friends") {
		t.Error("Selecting the same service twice should not report a change")
	}
	if !cfg.IsSelected("interim@friends") {
		t.Error("IsSelected should see the selected service")
	}
	if !cfg.DeselectService("interim@friends") {
		t.Error("DeselectService should report a change")
	}
	if cfg.DeselectService("interim@friends") {
		t.Error("Deselecting an unselected service should not report a change")
	}
}

func TestRemoveService(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SetCredential("interim@friends", "API_TOKEN", "interim_test")
	cfg.SelectService("interim@friends")

	if !cfg.RemoveService("interim@friends") {
		t.Fatal("RemoveService should report that it removed the entry")
	}
	if _, ok := cfg.Service("interim@friends"); ok {
		t.Error("the service entry should be gone")
	}
	if cfg.IsSelected("interim@friends") {
		t.Error("the service should be deselected")
	}
	if cfg.RemoveService("interim@friends") {
		t.Error("removing a missing service should not report a change")
	}
}

func TestToggleService(t *testing.T) {
	cfg := DefaultConfig()

	cfg.ToggleService("interim")
	if !cfg.IsSelected("interim") {
		t.Error("the first toggle should select the service")
	}
	cfg.ToggleService("interim")
	if cfg.IsSelected("interim") {
		t.Error("the second toggle should deselect the service")
	}
}
