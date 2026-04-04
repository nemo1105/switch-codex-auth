package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type idTokenClaims struct {
	Email   string `json:"email"`
	Profile *struct {
		Email string `json:"email"`
	} `json:"https://api.openai.com/profile"`
}

func extractComparableEmailFromAuthJSON(path string) (string, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false, fmt.Errorf("read %s: %w", path, err)
	}

	var auth authPayload
	if err := json.Unmarshal(data, &auth); err != nil {
		return "", false, nil
	}
	if auth.Tokens == nil {
		return "", false, nil
	}

	idToken := strings.TrimSpace(auth.Tokens.IDToken)
	if idToken == "" {
		return "", false, nil
	}

	email, err := extractEmailFromJWT(idToken)
	if err != nil {
		return "", false, nil
	}
	if email == "" {
		return "", false, nil
	}

	return strings.ToLower(email), true, nil
}

func extractEmailFromJWT(jwt string) (string, error) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("invalid JWT format")
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", err
	}

	var claims idTokenClaims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return "", err
	}

	email := strings.TrimSpace(claims.Email)
	if email != "" {
		return email, nil
	}
	if claims.Profile != nil {
		email = strings.TrimSpace(claims.Profile.Email)
		if email != "" {
			return email, nil
		}
	}

	return "", fmt.Errorf("email not found in JWT claims")
}
