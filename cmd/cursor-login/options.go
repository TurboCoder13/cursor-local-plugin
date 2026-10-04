package main

import (
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
)

type loginOptions struct {
	proxyURL  string
	keyFile   string
	noBrowser bool
}

func parseLoginOptions(args []string, getenv func(string) string, home string, output io.Writer) (loginOptions, error) {
	root := getenv("XDG_CONFIG_HOME")
	if root == "" {
		root = filepath.Join(home, ".config")
	}
	options := loginOptions{proxyURL: getenv("CLI_PROXY_URL"), keyFile: getenv("CLI_PROXY_MANAGEMENT_KEY_FILE")}
	if options.proxyURL == "" {
		options.proxyURL = "http://127.0.0.1:8317"
	}
	if options.keyFile == "" {
		options.keyFile = filepath.Join(root, "cliproxyapi", "management-key")
	}
	flags := flag.NewFlagSet("cursor-login", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&options.proxyURL, "url", options.proxyURL, "CPA URL (HTTPS or a loopback SSH tunnel)")
	flags.StringVar(&options.keyFile, "management-key-file", options.keyFile, "management key file; never put the key in argv")
	flags.BoolVar(&options.noBrowser, "no-browser", false, "print the temporary login URL for use on another machine")
	if err := flags.Parse(args); err != nil {
		return options, err
	}
	if flags.NArg() != 0 {
		return options, fmt.Errorf("unexpected positional arguments")
	}
	parsed, err := url.Parse(options.proxyURL)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return options, fmt.Errorf("invalid proxy URL")
	}
	loopback := parsed.Hostname() == "localhost" || parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "::1"
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && loopback) {
		return options, fmt.Errorf("use HTTPS or a loopback SSH tunnel for authenticated access")
	}
	return options, nil
}

func defaultLoginOptions() (loginOptions, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return loginOptions{}, fmt.Errorf("cannot find your home directory")
	}
	return parseLoginOptions(os.Args[1:], os.Getenv, home, os.Stderr)
}
