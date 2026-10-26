package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOnPath(t *testing.T) {
	sep := string(os.PathListSeparator)
	installDir := filepath.Join(string(os.PathSeparator), "Tools", "manyup")

	t.Setenv("PATH", strings.Join([]string{
		filepath.Join(string(os.PathSeparator), "Windows"),
		installDir,
		"",
	}, sep))

	if !onPath(installDir) {
		t.Errorf("onPath(%q) = false, want true", installDir)
	}
	if !onPath(installDir + string(os.PathSeparator)) {
		t.Error("onPath should ignore a trailing separator")
	}
	if !onPath(strings.ToUpper(installDir)) {
		t.Error("onPath should be case-insensitive")
	}
	if onPath(filepath.Join(string(os.PathSeparator), "Elsewhere")) {
		t.Error("onPath reported a directory that is not on PATH")
	}

	t.Setenv("PATH", "")
	if onPath(installDir) {
		t.Error("onPath should be false for an empty PATH")
	}
}

func TestParseSetArgs(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		wantService string
		wantKey     string
		wantValue   string
		wantErr     bool
	}{
		{
			name:        "short form defaults the key to API_TOKEN",
			args:        []string{"mocha", "mocha_abc123"},
			wantService: "mocha",
			wantKey:     "API_TOKEN",
			wantValue:   "mocha_abc123",
		},
		{
			name:        "explicit form",
			args:        []string{"mocha", "API_TOKEN", "mocha_abc123"},
			wantService: "mocha",
			wantKey:     "API_TOKEN",
			wantValue:   "mocha_abc123",
		},
		{
			name:        "explicit custom key",
			args:        []string{"vikingfile", "path", "/uploads/"},
			wantService: "vikingfile",
			wantKey:     "path",
			wantValue:   "/uploads/",
		},
		{
			name:        "service only means prompt",
			args:        []string{"storageto"},
			wantService: "storageto",
			wantKey:     "API_TOKEN",
			wantValue:   "",
		},
		{
			name:        "value with shell metacharacters is kept intact",
			args:        []string{"storageto", "896|UIVJ5F3mMOmetvdGT7BZat8PGCmtXeYDU5xJqgS7"},
			wantService: "storageto",
			wantKey:     "API_TOKEN",
			wantValue:   "896|UIVJ5F3mMOmetvdGT7BZat8PGCmtXeYDU5xJqgS7",
		},
		{name: "no arguments", args: nil, wantErr: true},
		{name: "empty value", args: []string{"mocha", ""}, wantErr: true},
		{name: "empty explicit value", args: []string{"mocha", "API_TOKEN", ""}, wantErr: true},
		{name: "too many arguments", args: []string{"mocha", "API_TOKEN", "a", "b"}, wantErr: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			service, key, value, err := parseSetArgs(c.args)
			if c.wantErr {
				if err == nil {
					t.Fatalf("parseSetArgs(%q) returned no error", c.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSetArgs(%q) returned %v", c.args, err)
			}
			if service != c.wantService || key != c.wantKey || value != c.wantValue {
				t.Errorf("parseSetArgs(%q) = (%q, %q, %q), want (%q, %q, %q)",
					c.args, service, key, value, c.wantService, c.wantKey, c.wantValue)
			}
		})
	}
}

func TestReadSecretValue(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{
			name:  "keeps shell metacharacters",
			input: "896|UIVJ5F3mMOmetvdGT7BZat8PGCmtXeYDU5xJqgS7\n",
			want:  "896|UIVJ5F3mMOmetvdGT7BZat8PGCmtXeYDU5xJqgS7",
		},
		{name: "strips CRLF", input: "token123\r\n", want: "token123"},
		{name: "keeps ampersands and carets", input: "a&b^c>d<e\n", want: "a&b^c>d<e"},
		{name: "no trailing newline", input: "token456", want: "token456"},
		{name: "empty input", input: "\n", wantErr: true},
		{name: "no input", input: "", wantErr: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := readSecretValue(strings.NewReader(c.input))
			if c.wantErr {
				if err == nil {
					t.Fatalf("readSecretValue(%q) returned %q, want an error", c.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("readSecretValue(%q) returned %v", c.input, err)
			}
			if got != c.want {
				t.Errorf("readSecretValue(%q) = %q, want %q", c.input, got, c.want)
			}
		})
	}
}

func TestIsCredentialKey(t *testing.T) {
	for _, key := range []string{"API_TOKEN", "API_KEY", "TOKEN"} {
		if !isCredentialKey(key) {
			t.Errorf("isCredentialKey(%q) = false, want true", key)
		}
	}
	for _, key := range []string{"path", "folder_id", "USERNAME", "PASSWORD", ""} {
		if isCredentialKey(key) {
			t.Errorf("isCredentialKey(%q) = true, want false", key)
		}
	}
}

func TestCompletionCandidates(t *testing.T) {
	contains := func(list []string, want string) bool {
		for _, v := range list {
			if v == want {
				return true
			}
		}
		return false
	}

	t.Run("first word", func(t *testing.T) {
		got := completionCandidates(nil)
		for _, want := range []string{"upload", "services", "config", "completion", "where"} {
			if !contains(got, want) {
				t.Errorf("candidates %v missing %q", got, want)
			}
		}
	})

	t.Run("config subcommands", func(t *testing.T) {
		got := completionCandidates([]string{"config"})
		for _, want := range []string{"set", "select", "show", "mode", "retry", "menu"} {
			if !contains(got, want) {
				t.Errorf("candidates %v missing %q", got, want)
			}
		}
	})

	t.Run("config set offers every service", func(t *testing.T) {
		got := completionCandidates([]string{"config", "set"})
		for _, want := range []string{"mocha", "gofile", "pixeldrain", "vikingfile", "zincdrive", "buzzheavier", "datanodes", "0x0"} {
			if !contains(got, want) {
				t.Errorf("candidates %v missing %q", got, want)
			}
		}
	})

	t.Run("config select offers every service", func(t *testing.T) {
		got := completionCandidates([]string{"config", "select"})
		if !contains(got, "mocha") {
			t.Errorf("candidates %v missing mocha", got)
		}
	})

	t.Run("config mode", func(t *testing.T) {
		got := completionCandidates([]string{"config", "mode"})
		if len(got) != 2 || got[0] != "parallel" || got[1] != "sequential" {
			t.Errorf("candidates = %v, want [parallel sequential]", got)
		}
	})

	t.Run("completion shells", func(t *testing.T) {
		got := completionCandidates([]string{"completion"})
		for _, want := range []string{"bash", "zsh", "fish", "powershell", "clink"} {
			if !contains(got, want) {
				t.Errorf("candidates %v missing %q", got, want)
			}
		}
	})

	t.Run("no candidates past a service name", func(t *testing.T) {
		if got := completionCandidates([]string{"upload", "file.bin"}); len(got) != 0 {
			t.Errorf("candidates = %v, want none", got)
		}
		if got := completionCandidates([]string{"config", "set", "mocha"}); len(got) != 0 {
			t.Errorf("candidates = %v, want none", got)
		}
	})
}

func TestCompletionScripts(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish", "powershell", "clink"} {
		script, ok := completionScript(shell)
		if !ok {
			t.Errorf("completionScript(%q) reported unsupported", shell)
			continue
		}
		if !strings.Contains(script, "__complete") {
			t.Errorf("completionScript(%q) does not call __complete", shell)
		}
		if !strings.Contains(script, "manyup") {
			t.Errorf("completionScript(%q) does not mention manyup", shell)
		}
	}

	if _, ok := completionScript("tcsh"); ok {
		t.Error("completionScript(tcsh) should be unsupported")
	}
}
