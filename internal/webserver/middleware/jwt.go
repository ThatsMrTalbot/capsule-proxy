package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/go-logr/logr"
	"github.com/gorilla/mux"
	authenticationv1 "k8s.io/api/authentication/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/clastix/capsule-proxy/internal/webserver/errors"
)

func CheckJWTMiddleware(client client.Client, log logr.Logger) mux.MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			var err error

			token := strings.ReplaceAll(request.Header.Get("Authorization"), "Bearer ", "")

			if len(token) > 0 {
				log.V(4).Info("Checking Bearer token", "value", token)
				tr := &authenticationv1.TokenReview{
					Spec: authenticationv1.TokenReviewSpec{
						Token: token,
					},
				}
				if err = client.Create(context.Background(), tr); err != nil {
					errors.HandleError(writer, err, "cannot create TokenReview")
				}
				log.V(5).Info("TokenReview", "value", tr.String())
				if statusErr := tr.Status.Error; len(statusErr) > 0 {
					errors.HandleError(writer, err, "cannot verify the token due to error")
				}
			}
			next.ServeHTTP(writer, request)
		})
	}
}
