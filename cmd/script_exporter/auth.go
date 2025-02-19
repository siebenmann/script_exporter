package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/dgrijalva/jwt-go"
)

func use(h http.HandlerFunc, middleware ...func(http.HandlerFunc) http.HandlerFunc) http.HandlerFunc {
	for _, m := range middleware {
		h = m(h)
	}

	return h
}

func auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Basic authentication
		if exporterConfig.BasicAuth.Active {
			w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)

			username, password, authOK := r.BasicAuth()
			if !authOK {
				http.Error(w, "Not authorized", http.StatusUnauthorized)
				return
			}

			if username != exporterConfig.BasicAuth.Username || password != exporterConfig.BasicAuth.Password {
				http.Error(w, "Not authorized", http.StatusUnauthorized)
				return
			}
		}

		// Authentication using bearer token
		if exporterConfig.BearerAuth.Active {
			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				http.Error(w, "Not authorized", http.StatusUnauthorized)
				return
			}

			authHeaderParts := strings.Split(authHeader, " ")
			if len(authHeaderParts) != 2 || strings.ToLower(authHeaderParts[0]) != "bearer" {
				http.Error(w, "Not authorized", http.StatusUnauthorized)
				return
			}

			err := checkJWT(authHeaderParts[1])
			if err != nil {
				http.Error(w, "Not authorized", http.StatusUnauthorized)
				return
			}
		}

		h.ServeHTTP(w, r)
	}
}

// checkJWT validates jwt tokens
func checkJWT(jwtToken string) error {
	token, err := jwt.Parse(jwtToken, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}

		return []byte(exporterConfig.BearerAuth.SigningKey), nil
	})

	if err != nil {
		return err
	}

	if _, ok := token.Claims.(jwt.MapClaims); ok && token.Valid {
		return nil
	}

	return errors.New("not authorized")
}

// createJWT creates jwt tokens
func createJWT() (string, error) {
	token := jwt.New(jwt.SigningMethodHS256)
	tokenString, err := token.SignedString([]byte(exporterConfig.BearerAuth.SigningKey))
	return tokenString, err
}
