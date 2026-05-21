package router

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/bobbyo/ccr/config"
	"github.com/bobbyo/ccr/db"
	"github.com/bobbyo/ccr/middleware"
	"github.com/bobbyo/ccr/providers"
)

// Router handles incoming LLM proxy requests.
type Router struct {
	cfg      *config.Manager
	database *sql.DB
	bc       *middleware.Broadcaster
	ks       *keyState
}

// New creates a new Router.
func New(cfg *config.Manager, database *sql.DB, bc *middleware.Broadcaster) *Router {
	return &Router{cfg: cfg, database: database, bc: bc, ks: newKeyState()}
}

// HandleMessages handles POST /v1/messages (Anthropic format in, Anthropic format out).
func (rt *Router) HandleMessages(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		jsonError(w, "failed to read request body", http.StatusBadRequest)
		return
	}

	req, err := AnthropicBodyToRequest(body)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.AnthropicBeta = r.Header.Get("anthropic-beta")

	rt.proxy(w, r, req, false)
}

// HandleChatCompletions handles POST /v1/chat/completions (OpenAI format in, OpenAI format out).
func (rt *Router) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		jsonError(w, "failed to read request body", http.StatusBadRequest)
		return
	}

	req, err := OpenAIToAnthropic(body)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	rt.proxy(w, r, req, true)
}

// HandleModels handles GET /v1/models — returns the list of configured model aliases.
func (rt *Router) HandleModels(w http.ResponseWriter, r *http.Request) {
	cfg := rt.cfg.Get()
	type modelObj struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	type modelsResp struct {
		Object string     `json:"object"`
		Data   []modelObj `json:"data"`
	}
	resp := modelsResp{Object: "list"}
	now := time.Now().Unix()
	for _, m := range cfg.Models {
		resp.Data = append(resp.Data, modelObj{
			ID:      m.Alias,
			Object:  "model",
			Created: now,
			OwnedBy: "ccr",
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// lazyStreamWriter defers SSE headers until the first byte is written.
// If all providers fail before writing anything, proxy() can still return a 503.
type lazyStreamWriter struct {
	rw      http.ResponseWriter
	flusher http.Flusher
	started bool
}

func newLazyStreamWriter(w http.ResponseWriter) *lazyStreamWriter {
	lw := &lazyStreamWriter{rw: w}
	if f, ok := w.(http.Flusher); ok {
		lw.flusher = f
	}
	return lw
}

func (lw *lazyStreamWriter) Write(p []byte) (int, error) {
	if !lw.started {
		lw.started = true
		lw.rw.Header().Set("Content-Type", "text/event-stream")
		lw.rw.Header().Set("Cache-Control", "no-cache")
		lw.rw.Header().Set("Connection", "keep-alive")
		lw.rw.WriteHeader(http.StatusOK)
	}
	n, err := lw.rw.Write(p)
	if lw.flusher != nil {
		lw.flusher.Flush()
	}
	return n, err
}

// proxy is the shared implementation for both endpoints.
// openAIOut=true means convert the Anthropic SSE response to OpenAI SSE format.
func (rt *Router) proxy(w http.ResponseWriter, r *http.Request, req providers.AnthropicRequest, openAIOut bool) {
	slog.Info("incoming", "model", req.Model)
	
	start := time.Now()
	cfg := rt.cfg.Get()

	clientKey := ""
	if k, ok := r.Context().Value(middleware.ClientKeyContextKey).(*db.ClientKey); ok && k != nil {
		clientKey = k.ID
	}
	requestedModel := req.Model

	// Non-streaming path: buffer SSE internally, then synthesize a plain JSON Message.
	// The classifier and other non-streaming callers expect a JSON body, not SSE.
	if req.NonStreaming && !openAIOut {
		var buf bytes.Buffer
		result, err := rt.fallbackChain(r.Context(), cfg, req, &buf, 0)
		if err != nil {
			slog.Error("router: all providers exhausted", "model", requestedModel, "err", err)
			jsonError(w, "all providers exhausted, please try again later", http.StatusServiceUnavailable)
			rt.publishEvent(middleware.LogEvent{
				TS:             time.Now().Unix(),
				ClientKey:      clientKey,
				RequestedModel: requestedModel,
				RoutedModel:    requestedModel,
				Provider:       "none",
				LatencyMS:      time.Since(start).Milliseconds(),
				Status:         http.StatusServiceUnavailable,
			})
			return
		}
		msgJSON, synthErr := sseBytesToMessage(buf.Bytes())
		if synthErr != nil {
			slog.Error("router: sse synthesis failed", "err", synthErr)
			jsonError(w, "failed to synthesize response", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(msgJSON)

		latency := time.Since(start).Milliseconds()
		_ = db.WriteUsage(rt.database, db.UsageRecord{
			Ts:             time.Now().Unix(),
			ClientKey:      clientKey,
			RequestedModel: requestedModel,
			RoutedModel:    result.RoutedModel,
			Provider:       result.Provider,
			InputTokens:    int64(result.InputTokens),
			OutputTokens:   int64(result.OutputTokens),
			LatencyMs:      latency,
			Status:         http.StatusOK,
			FallbackCount:  result.FallbackCount,
		})
		rt.publishEvent(middleware.LogEvent{
			TS:             time.Now().Unix(),
			ClientKey:      clientKey,
			RequestedModel: requestedModel,
			RoutedModel:    result.RoutedModel,
			Provider:       result.Provider,
			InputTokens:    result.InputTokens,
			OutputTokens:   result.OutputTokens,
			LatencyMS:      latency,
			Status:         http.StatusOK,
			FallbackCount:  result.FallbackCount,
		})
		return
	}

	lw := newLazyStreamWriter(w)

	var dest io.Writer
	var conv *openAIStreamConverter
	if openAIOut {
		conv = newOpenAIStreamConverter(lw)
		dest = conv
	} else {
		dest = lw
	}

	result, err := rt.fallbackChain(r.Context(), cfg, req, dest, 0)
	if err != nil {
		slog.Error("router: all providers exhausted", "model", requestedModel, "err", err)
		if !lw.started {
			jsonError(w, "all providers exhausted, please try again later", http.StatusServiceUnavailable)
		} else {
			// Headers already sent — emit an SSE error event so the client knows.
			fmt.Fprintf(lw, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"upstream connection lost\"}}\n\n")
		}
		rt.publishEvent(middleware.LogEvent{
			TS:             time.Now().Unix(),
			ClientKey:      clientKey,
			RequestedModel: requestedModel,
			RoutedModel:    requestedModel,
			Provider:       "none",
			LatencyMS:      time.Since(start).Milliseconds(),
			Status:         http.StatusServiceUnavailable,
		})
		return
	}

	latency := time.Since(start).Milliseconds()

	_ = db.WriteUsage(rt.database, db.UsageRecord{
		Ts:             time.Now().Unix(),
		ClientKey:      clientKey,
		RequestedModel: requestedModel,
		RoutedModel:    result.RoutedModel,
		Provider:       result.Provider,
		InputTokens:    int64(result.InputTokens),
		OutputTokens:   int64(result.OutputTokens),
		LatencyMs:      latency,
		Status:         http.StatusOK,
		FallbackCount:  result.FallbackCount,
	})

	rt.publishEvent(middleware.LogEvent{
		TS:             time.Now().Unix(),
		ClientKey:      clientKey,
		RequestedModel: requestedModel,
		RoutedModel:    result.RoutedModel,
		Provider:       result.Provider,
		InputTokens:    result.InputTokens,
		OutputTokens:   result.OutputTokens,
		LatencyMS:      latency,
		Status:         http.StatusOK,
		FallbackCount:  result.FallbackCount,
	})
}

// fallbackChain tries each provider for the given model alias, recursing into
// fallback_to if all providers fail.
//
// Providers write directly to w; ErrRateLimit and ErrUpstream are returned
// before any bytes are written, so the next provider can be tried safely.
func (rt *Router) fallbackChain(ctx context.Context, cfg *config.Config, req providers.AnthropicRequest, w io.Writer, depth int) (*FallbackResult, error) {
	if depth > 10 {
		return nil, ErrAllExhausted
	}

	// Claude Code appends a context-window annotation to model names, e.g. "opus[1m]"
	// or "sonnet[200k]". Strip it before alias lookup so the proxy config stays simple.
	alias := req.Model
	if i := strings.Index(alias, "["); i >= 0 {
		alias = alias[:i]
	}
	req.Model = alias

	var modelCfg *config.ModelConfig
	for i := range cfg.Models {
		if cfg.Models[i].Alias == req.Model {
			modelCfg = &cfg.Models[i]
			break
		}
	}
	if modelCfg == nil {
		return nil, fmt.Errorf("unknown model alias: %s", req.Model)
	}

	// fallbacks counts every key failure, provider failure, and model hop that
	// occurred before a successful response at this recursion level.
	var fallbacks int

	for _, mp := range modelCfg.Providers {
		provCfg, ok := cfg.Providers[mp.Provider]
		if !ok {
			slog.Warn("router: provider not configured", "provider", mp.Provider)
			continue
		}

		prov, err := newProvider(provCfg.Convention, provCfg.BaseURL)
		if err != nil {
			slog.Warn("router: cannot create provider", "provider", mp.Provider, "err", err)
			continue
		}

		apiKeys, err := db.GetProviderKeyValues(rt.database, mp.Provider)
		if err != nil {
			slog.Warn("router: failed to load keys for provider", "provider", mp.Provider, "err", err)
		}

		// Providers only write to w after a confirmed 200 response, so pre-stream
		// errors (429, 5xx) leave w untouched and we can safely try the next one.
		in, out, keyFallbacks, err := rt.tryProvider(ctx, prov, req, mp.ModelID, mp.Provider, apiKeys, w)
		fallbacks += keyFallbacks
		if err == nil {
			return &FallbackResult{
				RoutedModel:   mp.ModelID,
				Provider:      mp.Provider,
				InputTokens:   in,
				OutputTokens:  out,
				FallbackCount: fallbacks,
			}, nil
		}
		if errors.Is(err, providers.ErrRateLimit) || errors.Is(err, providers.ErrUpstream) {
			slog.Warn("router: provider failed, trying next", "provider", mp.Provider, "err", err)
			fallbacks++ // this whole provider was exhausted — one provider-level fallback
			continue
		}
		return nil, err
	}

	if modelCfg.FallbackTo != "" {
		slog.Info("router: falling back to model", "from", req.Model, "to", modelCfg.FallbackTo)
		req.Model = modelCfg.FallbackTo
		result, err := rt.fallbackChain(ctx, cfg, req, w, depth+1)
		if result != nil {
			// Add the fallbacks accumulated at this level plus one for the model hop itself.
			result.FallbackCount += fallbacks + 1
		}
		return result, err
	}

	return nil, ErrAllExhausted
}

// tryProvider attempts all API keys for one provider using round-robin rotation.
// Returns (inputTokens, outputTokens, keyFallbacks, error).
// keyFallbacks is the number of keys that made an actual HTTP request and failed
// (cooldown-skipped keys are not counted — no request was made).
func (rt *Router) tryProvider(
	ctx context.Context,
	prov providers.Provider,
	req providers.AnthropicRequest,
	modelID string,
	providerID string,
	apiKeys []string,
	w io.Writer,
) (int, int, int, error) {
	if len(apiKeys) == 0 {
		in, out, err := prov.Stream(ctx, req, modelID, "", w)
		return in, out, 0, err
	}

	n := len(apiKeys)
	start := rt.ks.startIdx(providerID, n)

	var lastErr error
	var keyFallbacks int
	for i := 0; i < n; i++ {
		idx := (start + i) % n
		key := apiKeys[idx]

		if rt.ks.isRateLimited(key) {
			// Key is already cooling down from a previous request — no HTTP call made.
			slog.Debug("router: key in cooldown, skipping", "provider", providerID, "key_index", idx)
			lastErr = providers.ErrRateLimit
			continue
		}

		in, out, err := prov.Stream(ctx, req, modelID, key, w)
		if err == nil {
			rt.ks.advanceIdx(providerID, idx, n)
			return in, out, keyFallbacks, nil
		}
		if errors.Is(err, providers.ErrRateLimit) {
			var rle *providers.RateLimitError
			retryAfter := time.Duration(0)
			if errors.As(err, &rle) {
				retryAfter = rle.RetryAfter
			}
			slog.Warn("router: key got 429, trying next key", "provider", providerID, "key_index", idx, "cooldown", retryAfter)
			rt.ks.markRateLimited(key, retryAfter)
			keyFallbacks++
			lastErr = err
			continue
		}
		if errors.Is(err, providers.ErrUpstream) {
			slog.Warn("router: key got upstream error, trying next key", "provider", providerID, "key_index", idx, "err", err)
			keyFallbacks++
			lastErr = err
			continue // try next key before giving up on this provider
		}
		return 0, 0, keyFallbacks, err
	}
	if lastErr != nil {
		return 0, 0, keyFallbacks, lastErr
	}
	return 0, 0, keyFallbacks, providers.ErrUpstream
}

func (rt *Router) publishEvent(ev middleware.LogEvent) {
	if rt.bc != nil {
		rt.bc.Publish(ev)
	}
}

func newProvider(convention, baseURL string) (providers.Provider, error) {
	switch convention {
	case "anthropic":
		return providers.NewAnthropicProvider(baseURL), nil
	case "openai":
		return providers.NewOpenAIProvider(baseURL), nil
	default:
		return nil, fmt.Errorf("unknown convention: %s", convention)
	}
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = fmt.Fprintf(w, `{"error":%q}`, msg)
}
