package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const hookScript = `set -eu
source hooks/docker_config.sh
if docker_config_needed; then
  update_docker_config
fi`

func runHook(t *testing.T, home string, env ...string) error {
	t.Helper()
	for _, tool := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	cmd := exec.Command("bash", "-c", hookScript)
	cmd.Env = append([]string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("hook output: %s", out)
	}
	return err
}

func dockerConfig(t *testing.T, home, content string) string {
	t.Helper()
	path := filepath.Join(home, ".docker", "config.json")
	if content == "" {
		return path
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	err := os.WriteFile(path, []byte(content), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func readJSON(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v: %q", path, err, b)
	}
	return m
}

const newAuths = `{"new.example":{"auth":"bmV3"}}`

func TestHookCreatesConfig(t *testing.T) {
	home := t.TempDir()
	path := dockerConfig(t, home, "")
	err := runHook(t, home, "DOCKERCFG="+newAuths)
	if err != nil {
		t.Fatal(err)
	}
	got := readJSON(t, path)
	want := map[string]interface{}{
		"auths": map[string]interface{}{
			"new.example": map[string]interface{}{
				"auth": "bmV3"}},
		"experimental": "enabled",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("config %v, want %v", got, want)
	}
}

func TestHookLeavesExperimentalConfig(t *testing.T) {
	home := t.TempDir()
	orig := `{"auths":{"old.example":{"auth":"b2xk"}},` +
		`"experimental":"enabled"}`
	path := dockerConfig(t, home, orig)
	err := runHook(t, home, "DOCKERCFG="+newAuths)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != orig {
		t.Fatalf("config changed to %q", b)
	}
}

func TestHookMergesConfig(t *testing.T) {
	home := t.TempDir()
	path := dockerConfig(t, home,
		`{"auths":{"old.example":{"auth":"b2xk"}},`+
			`"credsStore":"pass"}`)
	err := runHook(t, home, "DOCKERCFG="+newAuths)
	if err != nil {
		t.Fatal(err)
	}
	got := readJSON(t, path)
	auths, _ := got["auths"].(map[string]interface{})
	if auths["old.example"] == nil || auths["new.example"] == nil {
		t.Fatalf("auths not merged: %v", got)
	}
	if got["credsStore"] != "pass" || got["experimental"] != "enabled" {
		t.Fatalf("config %v", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("config mode %v", info.Mode())
	}
}

func TestHookWithoutDockerCfgDoesNothing(t *testing.T) {
	cases := []string{"",
		`{"auths":{"old.example":{"auth":"b2xk"}}}`,
		`{"experimental":"enabled"}`}
	for _, orig := range cases {
		home := t.TempDir()
		path := dockerConfig(t, home, orig)
		if err := runHook(t, home); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(path)
		if orig == "" {
			if !os.IsNotExist(err) {
				t.Fatalf("config created: %q", b)
			}
			continue
		}
		if string(b) != orig {
			t.Fatalf("config %q changed to %q", orig, b)
		}
	}
}

func TestHookBadInputKeepsConfig(t *testing.T) {
	cases := []struct{ name, orig, cfg string }{
		{"bad DOCKERCFG", `{"auths":{}}`, "not json"},
		{"bad config", `not json`, newAuths},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			path := dockerConfig(t, home, c.orig)
			err := runHook(t, home, "DOCKERCFG="+c.cfg)
			if err == nil {
				t.Fatal("hook succeeded on bad input")
			}
			b, _ := os.ReadFile(path)
			if string(b) != c.orig {
				t.Fatalf("config changed to %q", b)
			}
			left, _ := filepath.Glob(path + ".*")
			if len(left) != 0 {
				t.Fatalf("temp files left: %v", left)
			}
		})
	}
}

func TestPostCheckoutUsesHelpers(t *testing.T) {
	b, err := os.ReadFile("hooks/post_checkout")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		"source hooks/docker_config.sh",
		"if docker_config_needed; then",
		"update_docker_config",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("post_checkout lacks %q", want)
		}
	}
	if strings.Contains(s, "sponge ~/.docker/config.json") {
		t.Fatal("post_checkout still pipes into sponge")
	}
}
