package main

import (
	"io"
	"strings"
	"testing"
)

func TestLoginOptionsPortablePathsAndHeadless(t *testing.T) {
	env := map[string]string{"XDG_CONFIG_HOME": "/custom/config", "CLI_PROXY_URL": "http://127.0.0.1:18317"}
	options, err := parseLoginOptions([]string{"--no-browser"}, func(key string) string { return env[key] }, "/custom/home", io.Discard)
	if err != nil || !options.noBrowser || options.keyFile != "/custom/config/cliproxyapi/management-key" || options.proxyURL != env["CLI_PROXY_URL"] {
		t.Fatalf("portable options failed: %v", err)
	}
	override, err := parseLoginOptions([]string{"--url", "https://proxy.example.test", "--management-key-file", "/keys/proxy"}, func(string) string { return "" }, "/home/example", io.Discard)
	if err != nil || override.keyFile != "/keys/proxy" || override.proxyURL != "https://proxy.example.test" {
		t.Fatalf("explicit overrides failed: %v", err)
	}
}

func TestLoginRejectsUnsafeProxyDestinations(t *testing.T) {
	for _, destination := range []string{"http://proxy.example.test", "https://key@example.test", "https://example.test?secret=x", "https://example.test#fragment", "file:///tmp/key"} {
		_, err := parseLoginOptions([]string{"--url", destination}, func(string) string { return "" }, "/home/example", io.Discard)
		if err == nil {
			t.Errorf("unsafe destination accepted: %s", destination)
		}
	}
}

func TestPlatformBrowserLaunch(t *testing.T) {
	for _, goos := range []string{"darwin", "linux", "freebsd", "windows"} {
		command, err := browserCommand(goos, "https://cursor.com/loginDeepControl?fixture=true")
		if err != nil || command == nil || !strings.HasPrefix(command.Args[len(command.Args)-1], "https://cursor.com/") {
			t.Errorf("browser command failed for %s", goos)
		}
	}
	if _, err := browserCommand("unsupported", "https://cursor.com/"); err == nil {
		t.Error("unsupported browser platform accepted")
	}
}
