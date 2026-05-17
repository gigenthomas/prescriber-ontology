package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"time"

	lru "github.com/hashicorp/golang-lru/v2/expirable"
)

// ── Config ──────────────────────────────────────────────────────────────────

var (
	opaURL       = getenv("OPA_URL", "http://localhost:8181")
	opaCache     *lru.LRU[string, opaDecision]
	opaClient    = &http.Client{Timeout: 2 * time.Second}
)

// opaDecision is what the policy returns: a yes/no plus a human-readable
// reason (always populated by our Rego rules).
type opaDecision struct {
	Allow  bool   `json:"allow"`
	Reason string `json:"reason"`
}

func initOPA() {
	opaCache = lru.NewLRU[string, opaDecision](2048, nil, 30*time.Second)
}

// ── Decision API ───────────────────────────────────────────────────────────

// decide asks OPA "is this user allowed to call this tool/action?" The
// `packagePath` is the Rego package, e.g. "ontology/tools" or
// "ontology/actions". The `input` is whatever the policy expects.
//
// Result is cached for 30s, keyed by (packagePath + canonical(input)).
// Cache misses do an HTTP POST to OPA. Errors fall back to deny-by-default
// with a structured reason — never silently allow.
func decide(ctx context.Context, packagePath string, input map[string]any) opaDecision {
	if opaCache == nil {
		initOPA()
	}
	key := cacheKey(packagePath, input)
	if d, ok := opaCache.Get(key); ok {
		return d
	}

	body, err := json.Marshal(map[string]any{"input": input})
	if err != nil {
		log.Printf("[opa] marshal: %v", err)
		return opaDecision{Allow: false, Reason: "policy engine: encode error"}
	}

	url := opaURL + "/v1/data/" + packagePath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return opaDecision{Allow: false, Reason: "policy engine: request build error"}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := opaClient.Do(req)
	if err != nil {
		log.Printf("[opa] %s: %v", url, err)
		return opaDecision{Allow: false, Reason: "policy engine unreachable"}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		log.Printf("[opa] %s status=%d body=%s", url, resp.StatusCode, string(raw))
		return opaDecision{Allow: false, Reason: fmt.Sprintf("policy engine status %d", resp.StatusCode)}
	}

	var envelope struct {
		Result opaDecision `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		log.Printf("[opa] decode %s: %v", url, err)
		return opaDecision{Allow: false, Reason: "policy engine: malformed response"}
	}
	if envelope.Result.Reason == "" {
		envelope.Result.Reason = "no reason provided by policy"
	}

	opaCache.Add(key, envelope.Result)
	return envelope.Result
}

// ── Cache key helper ───────────────────────────────────────────────────────

func cacheKey(packagePath string, input map[string]any) string {
	// Canonicalize: SHA-256 over sorted JSON keys gives stable cache hits.
	canonical, _ := canonicalJSON(input)
	h := sha256.Sum256(append([]byte(packagePath+":"), canonical...))
	return hex.EncodeToString(h[:])
}

func canonicalJSON(v any) ([]byte, error) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var buf bytes.Buffer
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, _ := json.Marshal(k)
			buf.Write(kb)
			buf.WriteByte(':')
			child, err := canonicalJSON(t[k])
			if err != nil {
				return nil, err
			}
			buf.Write(child)
		}
		buf.WriteByte('}')
		return buf.Bytes(), nil
	case []any:
		var buf bytes.Buffer
		buf.WriteByte('[')
		for i, item := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			child, err := canonicalJSON(item)
			if err != nil {
				return nil, err
			}
			buf.Write(child)
		}
		buf.WriteByte(']')
		return buf.Bytes(), nil
	default:
		return json.Marshal(v)
	}
}

// ── Convenience wrappers used by tool/action dispatch ──────────────────────

// authorizeToolCall is called by executeTool before dispatch. Returns the
// decision; the caller is responsible for rejecting on !allow.
//
// Returns Allow=true unconditionally when auth is disabled (AUTH_PROVIDER=none).
func authorizeToolCall(ctx context.Context, toolName, inputJSON string) opaDecision {
	if !authEnabled() {
		return opaDecision{Allow: true, Reason: "auth disabled"}
	}
	user := userFromCtx(ctx)
	if user == nil {
		// Anonymous request to a protected path that somehow bypassed the
		// middleware. Fail closed.
		return opaDecision{Allow: false, Reason: "no authenticated user in context"}
	}

	// For action_* tools, also include parsed params so action-level policy
	// can read input.params (e.g. severity).
	in := map[string]any{
		"tool": toolName,
		"user": userPolicyView(user),
	}
	if len(toolName) > 7 && toolName[:7] == "action_" {
		var params map[string]any
		_ = json.Unmarshal([]byte(inputJSON), &params)
		in["action"] = toolName[7:]
		in["params"] = params
	}
	return decide(ctx, "ontology/tools", in)
}

// userPolicyView is what the Rego policy sees as input.user. Kept minimal
// (no tokens) so policies and audit logs can quote it safely.
func userPolicyView(u *AuthenticatedUser) map[string]any {
	return map[string]any{
		"sub":           u.Subject,
		"username":      u.Username,
		"email":         u.Email,
		"roles":         u.Roles,
		"authenticated": true,
	}
}
