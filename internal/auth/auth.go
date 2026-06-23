// Package auth obtiene tokens de acceso para Microsoft Graph mediante
// el flujo OAuth2 Client Credentials (autenticación app-only).
package auth

import (
	"context"
	"fmt"
	"net/http"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

// graphScope solicita todos los permisos de aplicación concedidos a la
// app registrada (el conjunto ".default" propio del flujo app-only).
const graphScope = "https://graph.microsoft.com/.default"

// tokenEndpoint construye la URL del endpoint de token para el tenant dado.
func tokenEndpoint(tenantID string) string {
	return fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", tenantID)
}

// NewConfig crea la configuración del flujo Client Credentials.
func NewConfig(tenantID, clientID, clientSecret string) *clientcredentials.Config {
	return &clientcredentials.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		TokenURL:     tokenEndpoint(tenantID),
		Scopes:       []string{graphScope},
		AuthStyle:    oauth2.AuthStyleInParams,
	}
}

// HTTPClient devuelve un *http.Client que inyecta automáticamente el token
// de acceso (y lo renueva cuando expira) en cada petición a Graph.
func HTTPClient(ctx context.Context, cfg *clientcredentials.Config) *http.Client {
	return cfg.Client(ctx)
}

// Token obtiene un token de acceso de forma explícita; útil para validar
// las credenciales antes de hacer llamadas reales a la API.
func Token(ctx context.Context, cfg *clientcredentials.Config) (*oauth2.Token, error) {
	tok, err := cfg.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("no se pudo obtener el token de Graph: %w", err)
	}
	return tok, nil
}
