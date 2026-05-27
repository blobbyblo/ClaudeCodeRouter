package admin

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/bobbyo/ccr/config"
	"github.com/bobbyo/ccr/db"
	"github.com/bobbyo/ccr/providers"
)

var (
	startTime     = time.Now()
	requestsTotal atomic.Int64
)

// IncrRequests increments the global request counter (called by the router).
func IncrRequests() { requestsTotal.Add(1) }

// ---- helpers -----------------------------------------------------------------

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = fmt.Fprintf(w, `{"error":%q}`+"\n", msg)
}

func parseIntParam(r *http.Request, name string, def int64) int64 {
	s := r.URL.Query().Get(name)
	if s == "" {
		return def
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return def
	}
	return v
}

// mintKeyID generates "sk-ccr-<24 hex chars>".
func mintKeyID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "sk-ccr-" + hex.EncodeToString(b), nil
}

// ---- /admin/api/info ---------------------------------------------------------

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Get()
	writeJSON(w, map[string]any{
		"client_port": cfg.Server.ClientPort,
	})
}

// ---- /admin/api/health -------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"status":         "ok",
		"uptime_s":       int64(time.Since(startTime).Seconds()),
		"requests_total": requestsTotal.Load(),
	})
}

// ---- /admin/api/keys ---------------------------------------------------------

func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := db.ListKeys(s.database)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type keyJSON struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		CreatedAt  int64  `json:"created_at"`
		LastUsedAt *int64 `json:"last_used_at"`
		Revoked    bool   `json:"revoked"`
	}
	out := make([]keyJSON, len(keys))
	for i, k := range keys {
		j := keyJSON{ID: k.ID, Name: k.Name, CreatedAt: k.CreatedAt, Revoked: k.Revoked}
		if k.LastUsed.Valid {
			j.LastUsedAt = &k.LastUsed.Int64
		}
		out[i] = j
	}
	writeJSON(w, out)
}

func (s *Server) handleMintKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
		jsonErr(w, "body must be JSON with a non-empty 'name' field", http.StatusBadRequest)
		return
	}

	id, err := mintKeyID()
	if err != nil {
		jsonErr(w, "failed to generate key", http.StatusInternalServerError)
		return
	}
	if err := db.CreateKey(s.database, id, body.Name); err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"id":   id,
		"name": body.Name,
		"note": "store this key — it will not be shown again",
	})
}

func (s *Server) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := db.RevokeKey(s.database, id); err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- /admin/api/usage --------------------------------------------------------

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	key := q.Get("key")
	model := q.Get("model")
	from := parseIntParam(r, "from", 0)
	to := parseIntParam(r, "to", 0)
	limit := int(parseIntParam(r, "limit", 100))

	rows, err := db.QueryUsage(s.database, key, model, from, to, limit)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, rows)
}

func (s *Server) handleUsageSummary(w http.ResponseWriter, r *http.Request) {
	rows, err := db.UsageSummary(s.database)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, rows)
}

func (s *Server) handleUsageLive(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, canFlush := w.(http.Flusher)
	fmt.Fprintf(w, ": connected\n\n")
	if canFlush {
		flusher.Flush()
	}

	ch := s.bc.Subscribe()
	defer s.bc.Unsubscribe(ch)

	for {
		select {
		case <-r.Context().Done():
			return
		case data, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			if canFlush {
				flusher.Flush()
			}
		}
	}
}

// ---- /admin/api/models -------------------------------------------------------

func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.cfg.Get().Models)
}

func (s *Server) handleUpsertModel(w http.ResponseWriter, r *http.Request) {
	var m config.ModelConfig
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil || m.Alias == "" {
		jsonErr(w, "invalid model body", http.StatusBadRequest)
		return
	}
	if err := s.cfg.UpsertModel(m, s.cfgPath); err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, m)
}

func (s *Server) handleDeleteModel(w http.ResponseWriter, r *http.Request) {
	alias := chi.URLParam(r, "alias")
	if err := s.cfg.DeleteModel(alias, s.cfgPath); err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- /admin/api/providers ----------------------------------------------------

func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	type provEntry struct {
		ID         string `json:"id"`
		BaseURL    string `json:"base_url"`
		Convention string `json:"convention"`
		KeyCount   int    `json:"key_count"`
	}
	var out []provEntry
	for id, p := range s.cfg.Get().Providers {
		cnt, _ := db.CountProviderKeys(s.database, id)
		out = append(out, provEntry{id, p.BaseURL, p.Convention, cnt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	writeJSON(w, out)
}

func (s *Server) handleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.cfg.DeleteProvider(id, s.cfgPath); err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleUpsertProvider(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID         string `json:"id"`
		BaseURL    string `json:"base_url"`
		Convention string `json:"convention"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		jsonErr(w, "invalid provider body", http.StatusBadRequest)
		return
	}
	pc := config.ProviderConfig{
		BaseURL:    body.BaseURL,
		Convention: body.Convention,
	}
	if err := s.cfg.UpsertProvider(body.ID, pc, s.cfgPath); err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, body)
}

// ---- /admin/api/providers/:id/keys ------------------------------------------

func (s *Server) handleListProviderKeys(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	keys, err := db.ListProviderKeys(s.database, id)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if keys == nil {
		keys = []db.ProviderKey{}
	}
	writeJSON(w, keys)
}

func (s *Server) handleAddProviderKey(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var body struct {
		KeyValue string `json:"key_value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.KeyValue == "" {
		jsonErr(w, "body must be JSON with a non-empty 'key_value' field", http.StatusBadRequest)
		return
	}
	rowID, err := db.AddProviderKey(s.database, id, body.KeyValue)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"id": rowID, "provider_id": id})
}

func (s *Server) handleDeleteProviderKey(w http.ResponseWriter, r *http.Request) {
	rawID := chi.URLParam(r, "keyId")
	var id int64
	if _, err := fmt.Sscan(rawID, &id); err != nil {
		jsonErr(w, "invalid key id", http.StatusBadRequest)
		return
	}
	if err := db.DeleteProviderKey(s.database, id); err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- /admin/api/test ---------------------------------------------------------

type TestResult struct {
	Model     string `json:"model"`
	Provider  string `json:"provider"`
	ModelID   string `json:"model_id"`
	KeyIndex  int    `json:"key_index"`
	KeyMasked string `json:"key_masked"`
	OK        bool   `json:"ok"`
	LatencyMs int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

func (s *Server) handleTest(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Get()

	// 60-second budget for the whole test run; each key gets 15 s.
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	baseReq := providers.AnthropicRequest{
		MaxTokens: 1,
		Stream:    true,
		Messages: []providers.Message{
			{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	}

	var results []TestResult

	for _, model := range cfg.Models {
		for _, mp := range model.Providers {
			provCfg, ok := cfg.Providers[mp.Provider]
			if !ok {
				continue
			}
			var prov providers.Provider
			switch provCfg.Convention {
			case "anthropic":
				prov = providers.NewAnthropicProvider(provCfg.BaseURL)
			case "openai":
				prov = providers.NewOpenAIProvider(provCfg.BaseURL)
			default:
				continue
			}

			keys, _ := db.GetProviderKeyValues(s.database, mp.Provider)
			if len(keys) == 0 {
				keys = []string{""}
			}

			for i, key := range keys {
				req := baseReq
				req.Model = mp.ModelID

				tCtx, tCancel := context.WithTimeout(ctx, 15*time.Second)
				start := time.Now()
				var buf bytes.Buffer
				_, _, err := prov.Stream(tCtx, req, mp.ModelID, key, &buf)
				latency := time.Since(start).Milliseconds()
				tCancel()

				res := TestResult{
					Model:     model.Alias,
					Provider:  mp.Provider,
					ModelID:   mp.ModelID,
					KeyIndex:  i,
					KeyMasked: maskKeyDisplay(key),
					LatencyMs: latency,
				}
				if err != nil {
					res.Error = err.Error()
				} else {
					res.OK = true
				}
				results = append(results, res)
			}
		}
	}

	if results == nil {
		results = []TestResult{}
	}
	writeJSON(w, results)
}

func maskKeyDisplay(k string) string {
	if k == "" {
		return "(no key)"
	}
	if len(k) <= 12 {
		return "••••••••"
	}
	return k[:8] + "…" + k[len(k)-4:]
}

// ---- /admin/api/restart ------------------------------------------------------

// handleRestart responds immediately then calls os.Exit(0) after a brief delay
// so NSSM (or any other service wrapper) automatically restarts the process,
// picking up a freshly-deployed binary without needing elevated privileges.
func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"status": "restarting"})
	go func() {
		time.Sleep(200 * time.Millisecond)
		os.Exit(0)
	}()
}

// ---- /admin/api/config/events ------------------------------------------------

func (s *Server) handleConfigEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, canFlush := w.(http.Flusher)
	fmt.Fprintf(w, ": connected\n\n")
	if canFlush {
		flusher.Flush()
	}

	ch := s.subscribeCfg()
	defer s.unsubscribeCfg(ch)

	for {
		select {
		case <-r.Context().Done():
			return
		case _, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "event: reload\ndata: {}\n\n")
			if canFlush {
				flusher.Flush()
			}
		}
	}
}

// ---- /admin/api/version ------------------------------------------------

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{
		"version": s.version,
	})
}

// ---- /admin/api/update ------------------------------------------------

// updateAsset builds the release asset file name for the current platform.
func updateAsset() string {
	var goos string
	switch runtime.GOOS {
	case "windows":
		goos = "Windows"
	case "linux":
		goos = "Linux"
	case "darwin":
		goos = "Darwin"
	default:
		goos = runtime.GOOS
	}
	var goarch string
	switch runtime.GOARCH {
	case "amd64":
		goarch = "x86_64"
	case "arm64":
		goarch = "arm64"
	default:
		goarch = runtime.GOARCH
	}
	if goos == "Windows" {
		return fmt.Sprintf("cc-router_%s_%s.zip", goos, goarch)
	}
	return fmt.Sprintf("cc-router_%s_%s.tar.gz", goos, goarch)
}

func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"status": "update check started"})
	go func() {
		apiClient := &http.Client{Timeout: 30 * time.Second}
		dlClient := &http.Client{Timeout: 10 * time.Minute}

		// fetch latest release from GitHub API
		resp, err := apiClient.Get("https://api.github.com/repos/blobbyblo/ClaudeCodeRouter/releases/latest")
		if err != nil {
			slog.Error("update: failed to check for latest release", "err", err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			slog.Error("update: GitHub API returned non-200", "status", resp.StatusCode)
			return
		}
		var rel struct {
			TagName string `json:"tag_name"`
			Assets  []struct {
				Name string `json:"name"`
				URL  string `json:"browser_download_url"`
			} `json:"assets"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
			slog.Error("update: failed to decode release JSON", "err", err)
			return
		}

		// compare tag (strip v prefix)
		latestVer := strings.TrimPrefix(rel.TagName, "v")
		curVer := strings.TrimPrefix(s.version, "v")
		if latestVer == curVer {
			slog.Info("update: already on latest version", "current", curVer, "latest", latestVer)
			return
		}

		// find asset for current platform
		assetName := updateAsset()
		var assetURL string
		for _, a := range rel.Assets {
			if a.Name == assetName {
				assetURL = a.URL
				break
			}
		}
		if assetURL == "" {
			slog.Error("update: no asset found for platform", "want", assetName)
			return
		}

		// download archive to a temp file
		archiveTmp, err := os.CreateTemp("", "cc-router-update-*")
		if err != nil {
			slog.Error("update: failed to create temp file", "err", err)
			return
		}
		archiveTmpName := archiveTmp.Name()
		defer os.Remove(archiveTmpName)

		resp2, err := dlClient.Get(assetURL)
		if err != nil {
			slog.Error("update: failed to download asset", "err", err)
			return
		}
		defer resp2.Body.Close()
		if resp2.StatusCode != http.StatusOK {
			slog.Error("update: asset download returned non-200", "status", resp2.StatusCode)
			return
		}
		if _, err := archiveTmp.ReadFrom(resp2.Body); err != nil {
			slog.Error("update: failed to write asset to temp file", "err", err)
			return
		}
		archiveTmp.Close()

		// locate current binary path
		execPath, err := os.Executable()
		if err != nil {
			slog.Error("update: failed to get executable path", "err", err)
			return
		}

		// extract binary into the same directory as the current executable so
		// that os.Rename can move it into place without a cross-device error.
		execDir := filepath.Dir(execPath)
		newTmp, err := os.CreateTemp(execDir, ".cc-router-new-*")
		if err != nil {
			slog.Error("update: failed to create staging file", "err", err)
			return
		}
		newTmpName := newTmp.Name()
		defer os.Remove(newTmpName)

		if err := extractBinary(archiveTmpName, newTmp, assetName); err != nil {
			newTmp.Close()
			slog.Error("update: failed to extract binary from archive", "err", err)
			return
		}
		newTmp.Close()

		if runtime.GOOS != "windows" {
			if err := os.Chmod(newTmpName, 0755); err != nil {
				slog.Error("update: failed to chmod new binary", "err", err)
				return
			}
		}

		// On Windows a running executable cannot be overwritten, but it can be
		// renamed.  Move the current binary aside first, then rename the new
		// one into place.  The .old file is cleaned up on next startup.
		if runtime.GOOS == "windows" {
			backupPath := execPath + ".old"
			_ = os.Remove(backupPath) // best-effort cleanup of a prior attempt
			if err := os.Rename(execPath, backupPath); err != nil {
				slog.Error("update: failed to move current binary aside", "err", err)
				return
			}
		}

		if err := os.Rename(newTmpName, execPath); err != nil {
			if runtime.GOOS == "windows" {
				_ = os.Rename(execPath+".old", execPath) // try to restore
			}
			slog.Error("update: failed to replace binary", "err", err)
			return
		}

		slog.Info("update: binary replaced; restarting", "from", curVer, "to", latestVer)
		time.Sleep(200 * time.Millisecond)
		os.Exit(0)
	}()
}

// extractBinary copies the cc-router binary out of a release archive into dst.
func extractBinary(archivePath string, dst *os.File, assetName string) error {
	if strings.HasSuffix(assetName, ".zip") {
		return extractZip(archivePath, dst)
	}
	return extractTarGz(archivePath, dst)
}

func extractZip(archivePath string, dst *os.File) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, ".exe") {
			rc, err := f.Open()
			if err != nil {
				return err
			}
			_, err = io.Copy(dst, rc)
			rc.Close()
			return err
		}
	}
	return fmt.Errorf("update: no .exe found in zip")
}

func extractTarGz(archivePath string, dst *os.File) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gr.Close()
	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag == tar.TypeReg && filepath.Base(hdr.Name) == "cc-router" {
			_, err = io.Copy(dst, tr)
			return err
		}
	}
	return fmt.Errorf("update: no binary found in tar.gz")
}

// ---- /admin/api/config/path ------------------------------------------------

func (s *Server) handleConfigPath(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{
		"path": s.cfgPath,
	})
}

// ---- /admin/api/config/raw ---------------------------------------------------

func (s *Server) handleGetRawConfig(w http.ResponseWriter, r *http.Request) {
	data, err := os.ReadFile(s.cfgPath)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Server) handleSetRawConfig(w http.ResponseWriter, r *http.Request) {
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r.Body); err != nil {
		jsonErr(w, "failed to read body", http.StatusBadRequest)
		return
	}
	if err := os.WriteFile(s.cfgPath, buf.Bytes(), 0644); err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// fsnotify will pick up the change and hot-reload.
	w.WriteHeader(http.StatusNoContent)
}
