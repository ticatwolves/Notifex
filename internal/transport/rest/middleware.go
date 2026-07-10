package rest

// import (
// 	"context"
// 	"net/http"

// 	"notifyhub/internal/domain"
// )

// type ctxKey string

// const appCtxKey ctxKey = "app"

// // apiKeyAuth validates the X-API-Key header against the service and injects
// // the resolved App into the request context. Bootstrap endpoints (creating
// // the app / api key itself) are mounted outside this middleware.
// func (h *Handler) apiKeyAuth(next http.Handler) http.Handler {
// 	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
// 		key := r.Header.Get("X-API-Key")
// 		if key == "" {
// 			writeError(w, http.StatusUnauthorized, "missing X-API-Key header")
// 			return
// 		}
// 		app, err := h.svc.ValidateAPIKey(r.Context(), key)
// 		if err != nil {
// 			writeError(w, http.StatusUnauthorized, "invalid API key")
// 			return
// 		}
// 		ctx := context.WithValue(r.Context(), appCtxKey, app)
// 		next.ServeHTTP(w, r.WithContext(ctx))
// 	})
// }

// func appFromContext(ctx context.Context) *domain.App {
// 	app, _ := ctx.Value(appCtxKey).(*domain.App)
// 	return app
// }
