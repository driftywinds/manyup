package secret

import (
	"encoding/json"
	"os"
	"testing"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := os.Setenv("XDG_CONFIG_HOME", dir); err != nil {
		t.Fatal(err)
	}
	defer os.Unsetenv("XDG_CONFIG_HOME")

	key, err := EnsureMasterKey()
	if err != nil {
		t.Fatal(err)
	}

	// Encrypt.
	enc, err := Encrypt(key, "my-secret-token")
	if err != nil {
		t.Fatal(err)
	}

	// Decrypt.
	plain, err := Decrypt(key, enc)
	if err != nil {
		t.Fatal(err)
	}
	if plain != "my-secret-token" {
		t.Fatalf("roundtrip mismatch: got %q", plain)
	}

	// Plaintext passthrough.
	pt, err := Decrypt(key, "just-a-plain-string")
	if err != nil {
		t.Fatal(err)
	}
	if pt != "just-a-plain-string" {
		t.Fatalf("passthrough mismatch: got %q", pt)
	}

	// Simulate Load(): mixed encrypted + plaintext in JSON.
	type SC struct {
		Credentials map[string]string `json:"credentials"`
	}
	jsonData := map[string]map[string]*SC{
		"services": {
			"datanodes": {
				Credentials: map[string]string{
					"API_TOKEN": enc,
					"extra":     "plain-value",
				},
			},
		},
	}
	data, _ := json.Marshal(jsonData)
	var cfg struct {
		Services map[string]*SC `json:"services"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}

	for _, sc := range cfg.Services {
		for k, v := range sc.Credentials {
			p, err := Decrypt(key, v)
			if err != nil {
				t.Fatalf("decrypt cred %s: %v", k, err)
			}
			sc.Credentials[k] = p
		}
	}

	if cfg.Services["datanodes"].Credentials["API_TOKEN"] != "my-secret-token" {
		t.Fatal("config decrypt wrong")
	}
	if cfg.Services["datanodes"].Credentials["extra"] != "plain-value" {
		t.Fatal("plaintext cred overwritten")
	}
}
