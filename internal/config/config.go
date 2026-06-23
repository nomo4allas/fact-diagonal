// Package config carga y valida la configuración de la aplicación
// a partir del archivo config.env.
package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/joho/godotenv"
)

// Config contiene las credenciales y parámetros operativos del agente.
type Config struct {
	TenantID       string // ID del tenant de Azure AD (Directory ID)
	ClientID       string // ID de la aplicación registrada (App registration)
	ClientSecret   string // Secreto del cliente
	Mailbox        string // Buzón a leer, p.ej. facturae@diagonal.com.co
	SimulationMode bool   // Si true, el agente solo lee y nunca modifica nada
}

// Load lee el archivo indicado (por defecto "config.env"), valida los
// campos obligatorios y devuelve una Config lista para usar.
func Load(path string) (*Config, error) {
	if path == "" {
		path = "config.env"
	}

	// godotenv.Load no sobrescribe variables ya presentes en el entorno,
	// lo que permite inyectar valores en despliegues sin tocar el archivo.
	if err := godotenv.Load(path); err != nil {
		return nil, fmt.Errorf("no se pudo cargar %s: %w", path, err)
	}

	cfg := &Config{
		TenantID:       strings.TrimSpace(os.Getenv("TENANT_ID")),
		ClientID:       strings.TrimSpace(os.Getenv("CLIENT_ID")),
		ClientSecret:   strings.TrimSpace(os.Getenv("CLIENT_SECRET")),
		Mailbox:        strings.TrimSpace(os.Getenv("MAILBOX")),
		SimulationMode: parseBool(os.Getenv("SIMULATION_MODE"), true),
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// validate comprueba que los campos obligatorios estén presentes.
func (c *Config) validate() error {
	missing := make([]string, 0, 4)
	if c.TenantID == "" {
		missing = append(missing, "TENANT_ID")
	}
	if c.ClientID == "" {
		missing = append(missing, "CLIENT_ID")
	}
	if c.ClientSecret == "" {
		missing = append(missing, "CLIENT_SECRET")
	}
	if c.Mailbox == "" {
		missing = append(missing, "MAILBOX")
	}
	if len(missing) > 0 {
		return fmt.Errorf("faltan variables obligatorias en config.env: %s", strings.Join(missing, ", "))
	}
	return nil
}

// parseBool interpreta valores comunes de verdadero/falso; ante un valor
// vacío o no reconocido devuelve el valor por defecto indicado.
func parseBool(v string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "si", "sí", "on":
		return true
	case "false", "0", "no", "off":
		return false
	default:
		return def
	}
}
