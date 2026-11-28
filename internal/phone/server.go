package phone

import (
	"context"
	"embed"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"time"
)

//go:embed static
var staticFiles embed.FS

// Config configures the Phone PWA server.
type Config struct {
	Store         Store
	Sessions      *SessionStore
	Now           func() time.Time
	SecureCookies bool
	ReadOnly      bool
}

// Handler returns the HTTP handler for the Phone PWA and its API.
func Handler(c Config) http.Handler {
	if c.Now == nil {
		c.Now = time.Now
	}

	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic("phone: embedded static folder missing: " + err.Error())
	}

	api := NewAPI(c.Store, c.Sessions, c.Now, c.SecureCookies, c.ReadOnly)
	mux := http.NewServeMux()

	// 1. Static Assets & PWA Shell
	fileServer := http.FileServerFS(sub)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch path {
		case "/", "/index.html":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		case "/manifest.webmanifest":
			w.Header().Set("Content-Type", "application/manifest+json")
			w.Header().Set("Cache-Control", "public, max-age=86400")
		case "/sw.js":
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		case "/app.js":
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		case "/app.css":
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		case "/icon.svg":
			w.Header().Set("Content-Type", "image/svg+xml")
			w.Header().Set("Cache-Control", "public, max-age=604800")
		default:
			if !strings.HasPrefix(path, "/api/") {
				// Fallback to index.html for client-side routing
				r.URL.Path = "/"
			}
		}
		fileServer.ServeHTTP(w, r)
	})

	// 2. Authentication Endpoints
	mux.HandleFunc("POST /api/phone/pair", api.HandlePair)
	mux.HandleFunc("GET /api/phone/session", api.HandleSession)
	mux.HandleFunc("POST /api/phone/logout", api.HandleLogout)

	// 3. Task Endpoints
	mux.HandleFunc("GET /api/phone/tasks", api.HandleListTasks)
	mux.HandleFunc("POST /api/phone/tasks", api.HandleCreateTask)
	mux.HandleFunc("POST /api/phone/tasks/{id}/status", api.HandleUpdateStatus)
	mux.HandleFunc("POST /api/phone/tasks/{id}/notes", api.HandleAddNote)
	mux.HandleFunc("POST /api/phone/tasks/{id}/delete", api.HandleDeleteTask)

	// 4. Attachments
	mux.HandleFunc("GET /api/phone/tasks/{id}/attachments/{hash}", api.HandleGetAttachment)
	mux.HandleFunc("POST /api/phone/tasks/{id}/attachments", api.HandleUploadAttachment)

	// 5. Suggestions
	mux.HandleFunc("GET /api/phone/suggestions", api.HandleListSuggestions)
	mux.HandleFunc("POST /api/phone/suggestions/{id}/{action}", api.HandleResolveSuggestion)

	// 6. Offline Queue Replay
	mux.HandleFunc("POST /api/phone/sync/replay", api.HandleReplayQueue)

	// Apply authentication and security middlewares
	handler := AuthMiddleware(c.Sessions, c.SecureCookies, mux)
	return SecurityMiddleware(handler)
}

// Serve runs the phone HTTP server on ln until ctx is canceled.
func Serve(ctx context.Context, ln net.Listener, c Config) error {
	srv := &http.Server{
		Handler:           Handler(c),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	done := make(chan error, 1)
	go func() {
		done <- srv.Serve(ln)
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}

	sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)

	if err := <-done; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
