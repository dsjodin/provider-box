package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dsjodin/provider-box/services/stepca-api/internal/store"
)

const (
	defaultLimit = 100
	maxLimit     = 1000
)

type Server struct {
	Store  *store.Store
	Token  string
	Logger *slog.Logger
}

func New(s *store.Store, token string, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{Store: s, Token: token, Logger: logger}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /certs", s.listCerts)
	mux.HandleFunc("GET /certs/{serial}", s.getCert)
	return s.requireAuth(mux)
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		ah := r.Header.Get("Authorization")
		if !strings.HasPrefix(ah, "Bearer ") {
			writeError(w, http.StatusUnauthorized, "missing or malformed Authorization header")
			return
		}
		provided := strings.TrimPrefix(ah, "Bearer ")
		// Constant-time compare; lengths must match for the compare to be meaningful.
		if len(provided) != len(s.Token) ||
			subtle.ConstantTimeCompare([]byte(provided), []byte(s.Token)) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type certResp struct {
	Serial       string     `json:"serial"`
	CommonName   string     `json:"common_name"`
	SANs         []string   `json:"sans"`
	Provisioner  string     `json:"provisioner,omitempty"`
	NotBefore    time.Time  `json:"not_before"`
	NotAfter     time.Time  `json:"not_after"`
	Status       string     `json:"status"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
	RevokeReason string     `json:"revoke_reason,omitempty"`
	Source       string     `json:"source,omitempty"`
}

func toResp(c store.Cert) certResp {
	sans := c.SANs
	if sans == nil {
		sans = []string{}
	}
	return certResp{
		Serial:       c.Serial,
		CommonName:   c.CommonName,
		SANs:         sans,
		Provisioner:  c.Provisioner,
		NotBefore:    c.NotBefore,
		NotAfter:     c.NotAfter,
		Status:       c.Status,
		RevokedAt:    c.RevokedAt,
		RevokeReason: c.RevokeReason,
		Source:       c.Source,
	}
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) listCerts(w http.ResponseWriter, r *http.Request) {
	f := store.ListFilter{Limit: defaultLimit}
	q := r.URL.Query()

	if v := q.Get("status"); v != "" {
		switch v {
		case store.StatusActive, store.StatusRevoked, store.StatusExpired:
			f.Status = v
		default:
			writeError(w, http.StatusBadRequest, "status must be one of: active, revoked, expired")
			return
		}
	}
	if v := q.Get("cn"); v != "" {
		f.CommonName = v
	}
	if v := q.Get("expiring_before"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "expiring_before must be RFC3339 (e.g. 2026-01-02T15:04:05Z)")
			return
		}
		f.ExpiringBefore = &t
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		if n > maxLimit {
			n = maxLimit
		}
		f.Limit = n
	}
	if v := q.Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "offset must be a non-negative integer")
			return
		}
		f.Offset = n
	}

	certs, err := s.Store.List(r.Context(), f)
	if err != nil {
		s.Logger.Error("list certs", "err", err)
		writeError(w, http.StatusInternalServerError, "store error")
		return
	}
	out := make([]certResp, 0, len(certs))
	for _, c := range certs {
		out = append(out, toResp(c))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getCert(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	if serial == "" {
		writeError(w, http.StatusBadRequest, "serial is required")
		return
	}
	c, err := s.Store.Get(r.Context(), serial)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "certificate not found")
			return
		}
		s.Logger.Error("get cert", "err", err, "serial", serial)
		writeError(w, http.StatusInternalServerError, "store error")
		return
	}
	writeJSON(w, http.StatusOK, toResp(*c))
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
