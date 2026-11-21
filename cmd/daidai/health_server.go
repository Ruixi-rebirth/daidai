package main

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"daidai/internal/adapter/qq"
	"daidai/internal/adapter/telegram"
	"daidai/internal/briefing"
	"daidai/internal/config"
	"daidai/internal/provider/llm"
)

func newHealthServer(serviceCtx context.Context, cfg *config.Config, qqChannel *qq.QQChannel, telegramChannel *telegram.Channel, router *llm.MindRouter, briefingService *briefing.Service, ociSchedulerDone <-chan struct{}) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte("ok"))
		}
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		checks := map[string]string{
			"qq_gateway":    "disabled",
			"llm_route":     "unconfigured",
			"oci_scheduler": "disabled",
		}
		ready := true
		if briefingService != nil {
			checks["geek_briefing"] = "stopped"
			if briefingService.Running.Load() {
				checks["geek_briefing"] = "running"
			} else {
				ready = false
			}
		}
		if cfg.Telegram.Enabled {
			checks["telegram"] = "not_ready"
			if telegramChannel != nil && telegramChannel.Ready() {
				checks["telegram"] = "ready"
			} else {
				ready = false
			}
		}
		if cfg.QQ.Enabled {
			checks["qq_gateway"] = "not_ready"
			if qqChannel != nil && qqChannel.Ready() {
				checks["qq_gateway"] = "ready"
			} else {
				ready = false
			}
		}
		if router.HasConfiguredRoute() {
			checks["llm_route"] = "configured"
		} else {
			ready = false
		}
		if cfg.OCI.Enabled {
			checks["oci_scheduler"] = "not_started"
			if ociSchedulerDone == nil {
				ready = false
			} else {
				select {
				case <-ociSchedulerDone:
					checks["oci_scheduler"] = "stopped"
					ready = false
				default:
					checks["oci_scheduler"] = "running"
				}
			}
		}
		if serviceCtx.Err() != nil {
			checks["lifecycle"] = "stopping"
			ready = false
		}
		status := http.StatusOK
		if !ready {
			status = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(struct {
				Ready  bool              `json:"ready"`
				Checks map[string]string `json:"checks"`
			}{Ready: ready, Checks: checks})
		}
	})

	return &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

}
