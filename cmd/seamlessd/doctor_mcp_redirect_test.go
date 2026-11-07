package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

// doctor's MCP probe follows no redirect (GO-2026-6911): a daemon whose
// endpoint redirects to another port of the same host -- where Go would keep
// the bearer key -- fails the probe, and the target is sent nothing.
func TestRemoteToolCount_RefusesARedirect(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
	}))
	defer target.Close()
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer daemon.Close()

	cfg := config.Defaults()
	cfg.AdvertisedURL = daemon.URL
	cfg.MCP.APIKey = "the-bearer-key"
	_, _, err := remoteToolCount(context.Background(), cfg)
	require.Error(t, err)
	require.Zero(t, hits.Load(), "the redirect's target received a request")
}
