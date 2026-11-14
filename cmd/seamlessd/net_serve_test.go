//go:build darwin || linux

package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Review M1 end to end: another process holding [::1]:P when serve starts on
// 127.0.0.1:P stops the start, naming the address. Before the fix serve ran
// on IPv4 alone with one warning, and http://localhost:P over IPv6 was the
// other process's.
func TestServe_RefusesToStartBesideASquattedLoopback(t *testing.T) {
	var ln4, squat net.Listener
	for range 20 {
		l4, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		_, port, _ := net.SplitHostPort(l4.Addr().String())
		l6, err := net.Listen("tcp", net.JoinHostPort("::1", port))
		require.NoError(t, l4.Close(), "serve binds the IPv4 half itself")
		if err == nil {
			ln4, squat = l4, l6
			break
		}
		if !netHasIPv6Loopback() {
			t.Skipf("no ::1 here: %v", err)
		}
	}
	require.NotNil(t, squat, "no port free on both loopback addresses")
	defer func() { _ = squat.Close() }()
	port := ln4.Addr().(*net.TCPAddr).Port

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	root := t.TempDir()
	cfgPath := filepath.Join(root, "seamless.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(fmt.Sprintf("addr: '127.0.0.1:%d'\ndata_dir: '%s'\nmcp:\n  api_key: '%s'\n"+
		"update:\n  check: false\ngardener:\n  enabled: false\n", port, filepath.Join(root, "data"), "net-serve-squat-key-0123456789")), 0o600))
	t.Setenv("SEAMLESS_CONFIG", cfgPath)

	done := make(chan error, 1)
	go func() { done <- runServe([]string{"--config", cfgPath}) }()
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			require.Error(t, err, "serve stopped without an error")
			addr := net.JoinHostPort("::1", strconv.Itoa(port))
			require.Contains(t, err.Error(), addr+" is in use by another process")
			require.Contains(t, err.Error(), "localhost is both loopback addresses")
			return
		default:
		}
		if resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port)); err == nil {
			_ = resp.Body.Close()
			// The old behavior: serve answers on IPv4 while [::1] is another
			// process's. Stop it, then fail.
			_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(20 * time.Second):
			}
			t.Fatalf("serve started on 127.0.0.1:%d while another process held [::1]:%d", port, port)
		}
		<-time.After(50 * time.Millisecond)
	}
	t.Fatal("serve neither refused nor answered")
}

// netHasIPv6Loopback reports whether this machine can listen on ::1 at all.
func netHasIPv6Loopback() bool {
	l, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}
