package main

import (
	"encoding/json"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/studiohallel/sofiaos/internal/auth"
	"github.com/studiohallel/sofiaos/internal/metrics"
	"github.com/studiohallel/sofiaos/internal/terminal"
)

// ── CONFIG ──────────────────────────────────────────────────────────────────

type config struct {
	addr        string
	galeneURL   string
	staticDir   string
	users       []userSeed
}

type userSeed struct {
	id, username, password, role string
}

func loadConfig() config {
	cfg := config{
		addr:      env("SOFIAOS_ADDR", ":8080"),
		galeneURL: env("SOFIAOS_GALENE_URL", "https://conference.studiohallel.online:8443"),
		staticDir: env("SOFIAOS_STATIC", "./web/static"),
	}

	// usuários definidos via env ou hardcoded para dev
	// produção: SOFIAOS_USERS="julios:senhaforte:admin,viewer:outrasenha:viewer"
	rawUsers := os.Getenv("SOFIAOS_USERS")
	if rawUsers != "" {
		for _, entry := range strings.Split(rawUsers, ",") {
			parts := strings.SplitN(entry, ":", 3)
			if len(parts) == 3 {
				cfg.users = append(cfg.users, userSeed{
					id:       parts[0],
					username: parts[0],
					password: parts[1],
					role:     parts[2],
				})
			}
		}
	} else {
		// dev seed — TROQUE antes de subir em produção
		cfg.users = []userSeed{
			{id: "1", username: "julios", password: "dev-troque-em-producao", role: "admin"},
		}
	}
	return cfg
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// ── MAIN ─────────────────────────────────────────────────────────────────────

func main() {
	cfg := loadConfig()

	store := auth.NewStore()
	for _, u := range cfg.users {
		if err := store.AddUser(u.id, u.username, u.password, u.role); err != nil {
			log.Fatalf("erro ao criar usuário %s: %v", u.username, err)
		}
	}

	mux := http.NewServeMux()

	// ── PUBLIC ──
	mux.HandleFunc("POST /api/auth/login", loginHandler(store))
	mux.HandleFunc("POST /api/auth/refresh", refreshHandler(store))

	// ── PROTECTED ──
	mux.Handle("GET /api/metrics", jwt(http.HandlerFunc(metricsHandler)))
	mux.Handle("GET /api/ws/terminal", jwt(http.HandlerFunc(terminal.Handler)))
	mux.Handle("/api/galene/", jwt(galeneProxy(cfg.galeneURL)))

	// ── STATIC (UI) ──
	mux.Handle("/", http.FileServer(http.Dir(cfg.staticDir)))

	srv := &http.Server{
		Addr:         cfg.addr,
		Handler:      cors(mux),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 0, // 0 = sem timeout para WebSocket
		IdleTimeout:  60 * time.Second,
	}

	log.Printf("sofiaos escutando em %s", cfg.addr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

// ── HANDLERS ─────────────────────────────────────────────────────────────────

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginResponse struct {
	Access  string `json:"access_token"`
	Refresh string `json:"refresh_token"`
	Role    string `json:"role"`
}

func loginHandler(store *auth.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req loginRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpError(w, "body inválido", http.StatusBadRequest)
			return
		}

		access, refresh, err := store.Authenticate(req.Username, req.Password)
		if err != nil {
			// mesmo erro para user inexistente e senha errada — evita enumeration
			time.Sleep(300 * time.Millisecond)
			httpError(w, "credenciais inválidas", http.StatusUnauthorized)
			return
		}

		// extrai role do access token para incluir na resposta
		claims, _ := auth.Verify(access)
		role := ""
		if claims != nil {
			role = claims.Role
		}

		jsonOK(w, loginResponse{Access: access, Refresh: refresh, Role: role})
	}
}

func refreshHandler(store *auth.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			httpError(w, "token ausente", http.StatusUnauthorized)
			return
		}
		claims, err := auth.Verify(token)
		if err != nil {
			httpError(w, "token inválido", http.StatusUnauthorized)
			return
		}
		// reemite apenas o access token
		newAccess, _, err := store.Authenticate("__refresh__", claims.UserID)
		_ = newAccess
		_ = err
		// simplificado: em produção valida o refresh token separadamente
		httpError(w, "use /api/auth/login", http.StatusNotImplemented)
	}
}

func metricsHandler(w http.ResponseWriter, r *http.Request) {
	snap, err := metrics.Collect()
	if err != nil {
		httpError(w, "erro ao coletar métricas: "+err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, snap)
}

func galeneProxy(target string) http.Handler {
	u, err := url.Parse(target)
	if err != nil {
		log.Fatalf("galene URL inválida: %v", err)
	}
	rp := httputil.NewSingleHostReverseProxy(u)
	return http.StripPrefix("/api/galene", rp)
}

// ── MIDDLEWARE ────────────────────────────────────────────────────────────────

// jwt valida o Bearer token em Authorization ou o cookie "sofiaos_token".
func jwt(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			// fallback: cookie (útil para WebSocket onde header é difícil)
			if c, err := r.Cookie("sofiaos_token"); err == nil {
				token = c.Value
			}
		}
		if token == "" {
			httpError(w, "não autenticado", http.StatusUnauthorized)
			return
		}
		claims, err := auth.Verify(token)
		if err != nil {
			httpError(w, "token inválido ou expirado", http.StatusUnauthorized)
			return
		}
		// injeta claims no header para uso downstream (sem context para manter simples)
		r.Header.Set("X-Sofia-User", claims.Username)
		r.Header.Set("X-Sofia-Role", claims.Role)
		next.ServeHTTP(w, r)
	})
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allowed := []string{
			"https://sofiaos.studiohallel.online",
			"https://studiohallel.online",
			"http://localhost:3000",
			"http://localhost:8080",
		}
		for _, a := range allowed {
			if origin == a {
				w.Header().Set("Access-Control-Allow-Origin", a)
				w.Header().Set("Access-Control-Allow-Credentials", "true")
				break
			}
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization,Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ── HELPERS ───────────────────────────────────────────────────────────────────

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return ""
}

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
