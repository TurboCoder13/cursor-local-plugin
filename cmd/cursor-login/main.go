// cursor-login uses the proxy's authenticated OAuth routes. Tokens stay in CPA.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"cursor-local-plugin/internal/provider"
)

func main() {
	if err := login(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func login() error {
	options, err := defaultLoginOptions()
	if err != nil {
		return err
	}
	key, err := os.ReadFile(options.keyFile)
	if err != nil {
		return fmt.Errorf("cannot read CLIProxyAPI management key")
	}
	client := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{}, CheckRedirect: func(*http.Request, []*http.Request) error {
		return fmt.Errorf("proxy redirect refused")
	}}
	defer client.CloseIdleConnections()
	get := func(path string) (map[string]any, error) {
		req, err := http.NewRequest(http.MethodGet, strings.TrimRight(options.proxyURL, "/")+"/v8/management/"+path, nil)
		if err != nil {
			return nil, fmt.Errorf("cannot construct proxy request")
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(key)))
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("cannot reach local CLIProxyAPI")
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("proxy returned HTTP %d; check that cursor-local is enabled", resp.StatusCode)
		}
		var result map[string]any
		if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result) != nil {
			return nil, fmt.Errorf("invalid proxy response")
		}
		return result, nil
	}
	start, err := get("oauth/auth-url?provider=" + provider.ID)
	if err != nil {
		return err
	}
	state, _ := start["state"].(string)
	loginURL, _ := start["url"].(string)
	parsed, err := url.Parse(loginURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "cursor.com" || parsed.Path != "/loginDeepControl" || state == "" {
		return fmt.Errorf("proxy returned an unexpected Cursor login destination")
	}
	defer func() {
		req, err := http.NewRequest(http.MethodDelete, strings.TrimRight(options.proxyURL, "/")+"/v8/management/oauth/session?state="+url.QueryEscape(state), nil)
		if err == nil {
			req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(key)))
			if resp, err := client.Do(req); err == nil {
				resp.Body.Close()
			}
		}
	}()
	if options.noBrowser {
		fmt.Println("Open this temporary Cursor login URL on your browser-equipped machine:")
		fmt.Println(loginURL)
	} else {
		fmt.Println("Opening Cursor login. Approve access in your browser.")
		command, err := browserCommand(runtime.GOOS, loginURL)
		if err != nil {
			return err
		}
		if err := command.Run(); err != nil {
			return fmt.Errorf("cannot open a browser; run cursor-login with --no-browser")
		}
	}
	deadline := time.Now().Add(9 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		status, err := get("oauth/status?state=" + url.QueryEscape(state))
		if err != nil {
			return err
		}
		switch status["status"] {
		case "ok":
			fmt.Println("Cursor connected. Its available models will appear as cursor/<model> in CLIProxyAPI.")
			return nil
		case "error":
			return fmt.Errorf("Cursor login failed; retry or check the local proxy logs")
		}
	}
	return fmt.Errorf("Cursor login timed out; run cursor-login again")
}

func browserCommand(goos, loginURL string) (*exec.Cmd, error) {
	switch goos {
	case "darwin":
		return exec.Command("open", loginURL), nil
	case "linux", "freebsd":
		return exec.Command("xdg-open", loginURL), nil
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", loginURL), nil
	default:
		return nil, fmt.Errorf("use --no-browser on this platform")
	}
}
