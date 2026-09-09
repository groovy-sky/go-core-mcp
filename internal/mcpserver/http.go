package mcpserver

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/groovy-sky/groovy-agent/internal/mcpproto"
)

// DefaultHTTPPath is the default endpoint path for the Streamable HTTP
// transport.
const DefaultHTTPPath = "/mcp"

// maxHTTPRequestBytes bounds the size of a single HTTP JSON-RPC request body,
// independent of the tool result limits, so an oversized request cannot
// exhaust server memory before it is even parsed.
const maxHTTPRequestBytes = 1 << 20 // 1 MiB

// HTTPOptions configures the Streamable HTTP transport handler.
type HTTPOptions struct {
	// Path is the single endpoint the handler answers on. Defaults to
	// DefaultHTTPPath when empty.
	Path string

	// BearerToken, when non-empty, requires every request to carry a bearer
	// authorization header matching this value (RFC 6750). This is the
	// minimum bar for publishing the endpoint beyond loopback: without a
	// token, anyone who can reach the listening address gets unauthenticated
	// access to the bounded filesystem tools.
	BearerToken string
}

// HTTPHandler returns an http.Handler implementing the MCP Streamable HTTP
// transport (protocol revision 2025-03-26) for a single endpoint path. The
// handler is stateless request/response only: this server never sends
// unsolicited (server-initiated) messages, so it does not open an SSE
// stream for GET requests and has no session to terminate via DELETE; both
// are answered with 405, which the specification permits for servers that do
// not support those optional capabilities.
func (s *Server) HTTPHandler(opts HTTPOptions) http.Handler {
	path := normalizeHTTPPath(opts.Path)
	mux := http.NewServeMux()
	mux.HandleFunc(path, s.handleHTTP(opts))
	return mux
}

// normalizeHTTPPath ensures the endpoint path is a well-formed http.ServeMux
// pattern (it must start with "/"), so a caller-supplied --http-path can
// never make HandleFunc panic at startup.
func normalizeHTTPPath(path string) string {
	if path == "" {
		return DefaultHTTPPath
	}
	if !strings.HasPrefix(path, "/") {
		return "/" + path
	}
	return path
}

func (s *Server) handleHTTP(opts HTTPOptions) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r, opts.BearerToken) {
			w.Header().Set("WWW-Authenticate", `Bearer`)
			http.Error(w, "missing or invalid bearer token", http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodPost:
			s.handleHTTPPost(w, r)
		default:
			// GET (server-initiated SSE stream) and DELETE (session
			// termination) are optional per the Streamable HTTP
			// specification; this server keeps no session state and never
			// pushes unsolicited messages, so it declines both with 405
			// rather than pretending to support them.
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

func authorized(r *http.Request, token string) bool {
	if token == "" {
		return true
	}
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	// The scheme name is case-insensitive per RFC 7235, so a client sending
	// "bearer <token>" must still be accepted. The prefix itself is public
	// (RFC 6750), so comparing it with a plain, non-constant-time check
	// leaks nothing secret; only the token itself is compared in constant
	// time below.
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return false
	}
	supplied := header[len(prefix):]
	return subtle.ConstantTimeCompare([]byte(supplied), []byte(token)) == 1
}

func (s *Server) handleHTTPPost(w http.ResponseWriter, r *http.Request) {
	// The Streamable HTTP transport requires POST bodies to be labeled
	// application/json; reject silently-untyped requests rather than
	// guessing, since we cannot verify the client actually sent JSON-RPC.
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}

	limited := io.LimitReader(r.Body, maxHTTPRequestBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	if len(body) > maxHTTPRequestBytes {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}

	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		http.Error(w, "empty JSON-RPC message", http.StatusBadRequest)
		return
	}
	if trimmed[0] == '[' {
		// JSON-RPC batching is not implemented; every request/notification
		// this server accepts is a single JSON object, matching the stdio
		// transport's newline-delimited-single-message behavior.
		http.Error(w, "batched JSON-RPC requests are not supported", http.StatusBadRequest)
		return
	}

	var buf bytes.Buffer
	request := mcpproto.Message{}
	if err := json.Unmarshal(trimmed, &request); err != nil {
		s.respondError(&buf, nil, mcpproto.CodeParseError, "malformed JSON-RPC message")
		writeJSONRPC(w, buf.Bytes())
		return
	}

	s.dispatch(r.Context(), &buf, request)
	if buf.Len() == 0 {
		// The request was a notification: the Streamable HTTP spec requires
		// no response body in that case.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeJSONRPC(w, buf.Bytes())
}

func writeJSONRPC(w http.ResponseWriter, message []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(bytes.TrimRight(message, "\n"))
}
